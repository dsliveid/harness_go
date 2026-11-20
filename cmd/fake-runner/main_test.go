package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"harness-go/internal/device"
)

// ============ fake-runner 的 fs.list 最小应答回归测试 ============
//
// fake-runner 是既有开发工具（ping/pong、task.dispatch 已稳定），这里只锁住本任务
// 新增的 fs.list 应答的四条契约（对齐对接约定 §3.3）：
//   1. separator 必须是宿主真实分隔符；
//   2. parent 到白名单根层必须是 nil，绝不返回 ".."；
//   3. dirs 只含目录（文件、软链一律不出现）；
//   4. with_roots 为真时回 roots，且越界如实报 path_denied、不存在报 path_not_found。

// newFSTestRunner 造一个以 dir 为唯一白名单根的 fakeRunner（不走网络）。
func newFSTestRunner(dir string) *fakeRunner {
	return &fakeRunner{
		cfg:  runnerConfig{fsRoots: dir},
		log:  func(string, ...any) {},
		done: make(map[string]device.TaskResultPayload),
	}
}

// TestFakeRunnerFsListRoots 未定位（path 空 / with_roots）→ 只回 roots。
func TestFakeRunnerFsListRoots(t *testing.T) {
	root := t.TempDir()
	r := newFSTestRunner(root)

	res := r.fsList(device.FsListPayload{Path: "", WithRoots: true})
	if len(res.Roots) != 1 {
		t.Fatalf("应回 1 个根，得到 %+v", res.Roots)
	}
	if res.Roots[0].Path != filepath.Clean(root) {
		t.Fatalf("根的 path 应为 %q，得到 %q", filepath.Clean(root), res.Roots[0].Path)
	}
	if res.Dirs == nil || len(res.Dirs) != 0 {
		t.Fatalf("未定位时不应给出子目录，得到 %+v", res.Dirs)
	}
	if res.Separator != string(filepath.Separator) {
		t.Fatalf("separator 必须是宿主分隔符 %q，得到 %q", string(filepath.Separator), res.Separator)
	}
	if res.Error != "" || res.Code != "" {
		t.Fatalf("正常应答不得带错误，得到 %q/%q", res.Error, res.Code)
	}
}

// TestFakeRunnerFsListOnlyDirs 只列目录，且根层 parent 必须为 nil。
func TestFakeRunnerFsListOnlyDirs(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"alpha", "Beta"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "readme.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	r := newFSTestRunner(root)

	res := r.fsList(device.FsListPayload{Path: root})
	if res.Error != "" {
		t.Fatalf("正常列举不应报错: %q", res.Error)
	}
	if len(res.Dirs) != 2 {
		t.Fatalf("应只返回 2 个目录，得到 %+v", res.Dirs)
	}
	for _, d := range res.Dirs {
		if strings.Contains(d.Name, ".") {
			t.Fatalf("文件项泄漏到目录列表: %q", d.Name)
		}
	}
	// 大小写不敏感排序（alpha / Beta）
	if res.Dirs[0].Name != "alpha" || res.Dirs[1].Name != "Beta" {
		t.Fatalf("排序不符预期: %+v", res.Dirs)
	}
	// 🚨 根层 parent 必须是 nil，绝不是 ".."
	if res.Parent != nil {
		t.Fatalf("白名单根层 parent 必须为 nil，得到 %q", *res.Parent)
	}
	if res.Path != filepath.Clean(root) {
		t.Fatalf("path 应为 %q，得到 %q", filepath.Clean(root), res.Path)
	}
}

// TestFakeRunnerFsListSubdirParent 子目录的 parent 指回根，且仍在白名单内。
func TestFakeRunnerFsListSubdirParent(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "Work")
	if err := os.MkdirAll(filepath.Join(sub, "Proj"), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	r := newFSTestRunner(root)

	res := r.fsList(device.FsListPayload{Path: sub})
	if res.Error != "" {
		t.Fatalf("正常列举不应报错: %q", res.Error)
	}
	if res.Parent == nil || filepath.Clean(*res.Parent) != filepath.Clean(root) {
		t.Fatalf("子目录 parent 应为根，得到 %v", res.Parent)
	}
	if len(res.Dirs) != 1 || res.Dirs[0].Name != "Proj" {
		t.Fatalf("应返回 Proj，得到 %+v", res.Dirs)
	}
	if res.Dirs[0].Path != filepath.Join(filepath.Clean(sub), "Proj") {
		t.Fatalf("目录项 path 应为绝对路径，得到 %q", res.Dirs[0].Path)
	}
}

// TestFakeRunnerFsListErrors 越界 / 不存在必须如实报错，绝不降级成空列表。
func TestFakeRunnerFsListErrors(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	r := newFSTestRunner(root)

	res := r.fsList(device.FsListPayload{Path: outside})
	if res.Code != "path_denied" || res.Error == "" {
		t.Fatalf("越界应回 path_denied，得到 code=%q err=%q", res.Code, res.Error)
	}

	res = r.fsList(device.FsListPayload{Path: filepath.Join(root, "not-exist")})
	if res.Code != "path_not_found" || res.Error == "" {
		t.Fatalf("不存在应回 path_not_found，得到 code=%q err=%q", res.Code, res.Error)
	}
}

// TestFakeRunnerFsListFrameRoundTrip 应答帧必须能按设备协议往返（服务端 reqID 配对依赖 id）。
func TestFakeRunnerFsListFrameRoundTrip(t *testing.T) {
	root := t.TempDir()
	r := newFSTestRunner(root)

	reqID := "fsq-1704067200000000000-1"
	res := r.fsList(device.FsListPayload{Path: "", WithRoots: true})
	env := device.MustEnvelope(device.MsgFsListResult, reqID, res)

	if env.Type != device.MsgFsListResult {
		t.Fatalf("帧类型应为 %q，得到 %q", device.MsgFsListResult, env.Type)
	}
	if env.ID != reqID {
		t.Fatalf("id 必须逐字回传请求的 id，得到 %q", env.ID)
	}
	if !env.Type.Valid() {
		t.Fatal("fs.list.result 必须是已知帧类型")
	}
	var back device.FsListResultPayload
	if err := env.Decode(&back); err != nil {
		t.Fatalf("负载解码失败: %v", err)
	}
	if back.Separator != string(filepath.Separator) {
		t.Fatalf("往返后 separator 应保持，得到 %q", back.Separator)
	}
	if len(back.Roots) != 1 {
		t.Fatalf("往返后 roots 应保持，得到 %+v", back.Roots)
	}
}

// TestFakeRunnerPlatformDefault 平台标识不参与目录列举，但改动默认值时应有意识。
func TestFakeRunnerPlatformDefault(t *testing.T) {
	if got := defaultPlatform(); got != "fake" {
		t.Fatalf("默认平台标识应为 fake，得到 %q", got)
	}
	if runtime.GOOS == "" {
		t.Skip("平台未知")
	}
}
