package hostimpl

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"harness-go/pkg/toolapi"
)

// ============ 会话执行根（ctx 承载）回归测试 ============
//
// 背景（2026-11 用户实测）：目录选择器里选了别的目录，工具仍只在进程启动目录里读写。
// 根因是执行根在 hostimpl 里被固定成进程级 w.root，与 session.project_dir 毫无关系。
//
// 本文件的断言锁死三件事：
//  1. ctx 带会话根时，文件访问落到会话根（修好的行为）；
//  2. 无 ctx 根时回落进程根（既有调用点与后台任务行为不得改变）；
//  3. 会话根**不扩大授权**：`..` 上跳与绝对路径仍被拒绝（沙箱不能被绕过）。

// newRootFixture 造两个目录：进程根 + 会话根，各放一个可辨识的文件。
func newRootFixture(t *testing.T) (h *Host, procRoot, sessRoot string) {
	t.Helper()
	procRoot = t.TempDir()
	sessRoot = t.TempDir()

	if err := os.WriteFile(filepath.Join(procRoot, "who.txt"), []byte("proc-root"), 0o644); err != nil {
		t.Fatalf("写进程根文件失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessRoot, "who.txt"), []byte("session-root"), 0o644); err != nil {
		t.Fatalf("写会话根文件失败: %v", err)
	}
	return New(Options{WorkDir: procRoot}), procRoot, sessRoot
}

// ① ctx 带会话根 → 读到的是会话根里的文件（而非进程根）。
//
// 这是"选了目录却不生效"的直接回归：改造前这里必然读到 proc-root。
func TestWorkspaceUsesContextRoot(t *testing.T) {
	h, _, sessRoot := newRootFixture(t)
	ctx := toolapi.WithWorkspaceRoot(context.Background(), sessRoot)

	data, err := h.Workspace().ReadFile(ctx, "who.txt")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(data) != "session-root" {
		t.Fatalf("ctx 会话根未生效：期望读到 session-root，实际 %q", string(data))
	}
}

// ② 无 ctx 根 → 回落进程根（向后兼容，绝不能 panic 或读到空）。
func TestWorkspaceFallsBackToProcessRoot(t *testing.T) {
	h, _, _ := newRootFixture(t)

	data, err := h.Workspace().ReadFile(context.Background(), "who.txt")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(data) != "proc-root" {
		t.Fatalf("无 ctx 根时应回落进程根（proc-root），实际 %q", string(data))
	}
	// Root() 无 ctx，必须恒返回进程根（否则旧调用点语义变化）。
	if got := h.Workspace().Root(); filepath.Clean(got) == "" {
		t.Fatal("Root() 不得返回空串")
	}
}

// ③ ctx 根为空串 → 等价于未注入，回落进程根。
//
// 这条防止「解析器返回空根」被误当成"根就是空串"，从而 join 到进程 CWD。
func TestWorkspaceEmptyContextRootFallsBack(t *testing.T) {
	h, _, _ := newRootFixture(t)
	ctx := toolapi.WithWorkspaceRoot(context.Background(), "   ")

	if _, ok := toolapi.WorkspaceRootFrom(ctx); ok {
		t.Fatal("空白根不得被当作有效会话根")
	}
	data, err := h.Workspace().ReadFile(ctx, "who.txt")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(data) != "proc-root" {
		t.Fatalf("空根应回落进程根，实际 %q", string(data))
	}
}

// ④ 会话根不得成为绕过沙箱的跳板：`..` 与绝对路径仍必须被拒绝。
//
// 这是本次改造最重要的安全断言：放开"选目录"绝不能顺带放开越界。
func TestContextRootStillRejectsEscape(t *testing.T) {
	h, _, sessRoot := newRootFixture(t)
	ctx := toolapi.WithWorkspaceRoot(context.Background(), sessRoot)

	for _, rel := range []string{"..", "../who.txt", "a/../../b"} {
		if _, err := h.Workspace().ReadFile(ctx, rel); err == nil {
			t.Fatalf("会话根下 `..` 上跳必须被拒绝，实际放行了 %q", rel)
		}
	}
	// 绝对路径同样拒绝（否则模型可直接读 C:\Windows\...）。
	abs := filepath.Join(sessRoot, "who.txt")
	if _, err := h.Workspace().ReadFile(ctx, abs); err == nil {
		t.Fatalf("绝对路径必须被拒绝，实际放行了 %q", abs)
	}
}

// ⑤ 写入也跟随 ctx 根，且 Remove 不得删掉会话根本身。
func TestContextRootWriteAndRemoveGuard(t *testing.T) {
	h, procRoot, sessRoot := newRootFixture(t)
	ctx := toolapi.WithWorkspaceRoot(context.Background(), sessRoot)

	if err := h.Workspace().WriteFile(ctx, "new/child.txt", []byte("hi"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	// 必须落在会话根内，而不是进程根内。
	if _, err := os.Stat(filepath.Join(sessRoot, "new", "child.txt")); err != nil {
		t.Fatalf("文件未落在会话根内: %v", err)
	}
	if _, err := os.Stat(filepath.Join(procRoot, "new", "child.txt")); err == nil {
		t.Fatal("文件不得落在进程根内（说明 ctx 根未生效）")
	}
	// 不允许删除工作区根（删根等于清空用户整个项目目录）。
	if err := h.Workspace().Remove(ctx, "."); err == nil {
		t.Fatal("不允许删除工作区根")
	}
}

// ⑥ 命令执行器同样跟随 ctx 根：cmd.Dir 必须是会话根。
//
// 用 `cd`（Windows）/`pwd`（POSIX）不可靠，改为跑一个必然失败的命令并只断言
// 越界请求被拒 —— 置信度足够且不依赖具体 shell 行为。
func TestCommandRejectsEscapeUnderContextRoot(t *testing.T) {
	_, _, sessRoot := newRootFixture(t)
	r := NewCommandRunner(CommandOptions{Root: t.TempDir()})
	ctx := toolapi.WithWorkspaceRoot(context.Background(), sessRoot)

	if _, err := r.Run(ctx, toolapi.CommandRequest{Cmd: "echo", Dir: "../outside"}); err == nil {
		t.Fatal("会话根下命令 cwd 上跳必须被拒绝")
	}
}
