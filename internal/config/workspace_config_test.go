package config

import (
	"reflect"
	"testing"
)

// ============ workspace.fs_roots 配置优先级（2026-11 目录选择放开）============
//
// 契约：HARNESS_FS_ROOTS（env） > config.yaml workspace.fs_roots > 默认（数据目录父级）。
//
// 之所以必须锁死这条：白名单是"放开选目录"与"主机文件读写面"之间唯一的闸门。
// 一旦优先级写错（例如 env 只能追加不能覆盖），运维在应急时改了环境变量却发现
// 不生效，会误判为"功能坏了"，进而去改代码而不是改配置。

// 默认值：不放开额外根，且强制白名单（安全默认，与改造前行为一致）。
func TestWorkspaceDefaultIsClosed(t *testing.T) {
	cfg := Default()
	if len(cfg.Workspace.FSRoots) != 0 {
		t.Fatalf("默认不应放开任何额外根，得到 %v", cfg.Workspace.FSRoots)
	}
	if !cfg.Workspace.EnforceFSRoots {
		t.Fatal("默认必须强制白名单（enforce_fs_roots=true）")
	}
}

// 环境变量可覆盖默认值，并按分号切分。
func TestWorkspaceFSRootsFromEnv(t *testing.T) {
	env := map[string]string{
		"HARNESS_FS_ROOTS": `F:\;D:\projects`,
	}
	cfg, err := Load(Options{
		Args: []string{"-data", t.TempDir()},
		Env:  func(k string) string { return env[k] },
	})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	want := []string{`F:\`, `D:\projects`}
	if !reflect.DeepEqual(cfg.Workspace.FSRoots, want) {
		t.Fatalf("fs_roots 切分错误：期望 %v，得到 %v", want, cfg.Workspace.FSRoots)
	}
}

// 环境变量提供时，空串应被跳过（避免把空项塞进白名单）。
func TestWorkspaceFSRootsEnvSkipsBlank(t *testing.T) {
	env := map[string]string{
		"HARNESS_FS_ROOTS": `F:\;;  ;D:\x`,
	}
	cfg, err := Load(Options{
		Args: []string{"-data", t.TempDir()},
		Env:  func(k string) string { return env[k] },
	})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	want := []string{`F:\`, `D:\x`}
	if !reflect.DeepEqual(cfg.Workspace.FSRoots, want) {
		t.Fatalf("空白项应被跳过：期望 %v，得到 %v", want, cfg.Workspace.FSRoots)
	}
}

// HARNESS_FS_ENFORCE_ROOTS=false 可显式关闭白名单强制。
func TestWorkspaceEnforceFromEnv(t *testing.T) {
	env := map[string]string{"HARNESS_FS_ENFORCE_ROOTS": "false"}
	cfg, err := Load(Options{
		Args: []string{"-data", t.TempDir()},
		Env:  func(k string) string { return env[k] },
	})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.Workspace.EnforceFSRoots {
		t.Fatal("HARNESS_FS_ENFORCE_ROOTS=false 应关闭强制白名单")
	}
}
