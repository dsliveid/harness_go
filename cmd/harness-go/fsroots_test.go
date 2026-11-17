package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestResolveFSRootsDefaultUnrestricted 未配置 fs_roots 时默认放开整机。
//
// 这是 2026-11 的口径变更：此前默认只有「数据目录父级」（启动目录），
// 用户在 Web 目录选择器里选不到服务器其他目录（fs.path_denied）。
func TestResolveFSRootsDefaultUnrestricted(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("建数据目录失败: %v", err)
	}

	roots, restricted := resolveFSRoots(dataDir, nil)
	if restricted {
		t.Fatalf("未配置 fs_roots 时应为「默认整机」(restricted=false)，得到 true")
	}
	if len(roots) == 0 {
		t.Fatalf("默认整机时根列表不能为空")
	}
	for _, r := range roots {
		if !filepath.IsAbs(r) {
			t.Fatalf("根必须是绝对路径: %q", r)
		}
	}
	if runtime.GOOS == "windows" {
		// 至少要包含当前盘（测试环境一定存在）。
		cur, _ := os.Getwd()
		want := strings.ToUpper(filepath.VolumeName(cur))
		found := false
		for _, r := range roots {
			if strings.EqualFold(filepath.VolumeName(r), want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("默认整机应包含当前盘 %q，实际 %v", want, roots)
		}
	} else {
		found := false
		for _, r := range roots {
			if r == "/" {
				found = true
			}
		}
		if !found {
			t.Fatalf("POSIX 默认整机应包含 /，实际 %v", roots)
		}
	}
}

// TestResolveFSRootsExplicitRestriction 显式配置 fs_roots 即进入受限模式。
func TestResolveFSRootsExplicitRestriction(t *testing.T) {
	dir := t.TempDir()
	roots, restricted := resolveFSRoots(filepath.Join(dir, "data"), []string{dir})
	if !restricted {
		t.Fatalf("配置了 fs_roots 应为受限模式 (restricted=true)")
	}
	if len(roots) != 1 || roots[0] != filepath.Clean(dir) {
		t.Fatalf("受限模式根应为 [%q]，实际 %v", filepath.Clean(dir), roots)
	}
}

// TestResolveFSRootsInvalidOnly 配置项全部无效时返回空（失败即关闭，绝不静默放开整机）。
func TestResolveFSRootsInvalidOnly(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-exist-dir")
	roots, restricted := resolveFSRoots(t.TempDir(), []string{missing})
	if !restricted {
		t.Fatalf("配置了 fs_roots（即便无效）也应标记为受限模式")
	}
	if len(roots) != 0 {
		t.Fatalf("全部无效时应返回空根（上层明确报错），实际 %v", roots)
	}
}
