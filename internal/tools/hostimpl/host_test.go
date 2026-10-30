package hostimpl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"harness-go/pkg/toolapi"
)

func TestWorkspaceSandbox(t *testing.T) {
	root := t.TempDir()
	h := New(Options{WorkDir: root})
	ws := h.Workspace()
	if ws.Root() != root {
		t.Fatalf("工作区根错误: %s", ws.Root())
	}

	// 正常读写
	if err := ws.WriteFile(context.Background(), "sub/dir/a.txt", []byte("hello"), 0o644); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	data, err := ws.ReadFile(context.Background(), "sub/dir/a.txt")
	if err != nil || string(data) != "hello" {
		t.Fatalf("读取失败: %v %q", err, data)
	}
	exists, err := ws.Exists(context.Background(), "sub/dir/a.txt")
	if err != nil || !exists {
		t.Fatalf("Exists 应为 true: %v %v", exists, err)
	}

	// 越界与绝对路径必须拒绝
	if _, err := ws.Resolve("../escape.txt"); !errors.Is(err, toolapi.ErrInvalidArgs) {
		t.Fatalf("越界路径应拒绝，得到 %v", err)
	}
	abs := filepath.Join(root, "abs.txt")
	if _, err := ws.Resolve(abs); !errors.Is(err, toolapi.ErrInvalidArgs) {
		t.Fatalf("绝对路径应拒绝，得到 %v", err)
	}
	if err := ws.Remove(context.Background(), ""); !errors.Is(err, toolapi.ErrInvalidArgs) {
		t.Fatalf("删除根应拒绝，得到 %v", err)
	}

	// 列表
	entries, err := ws.List(context.Background(), "sub/dir")
	if err != nil || len(entries) != 1 || entries[0].Path == "" || entries[0].Size != 5 {
		t.Fatalf("列表异常: %v %+v", err, entries)
	}
	stat, err := ws.Stat(context.Background(), "sub/dir/a.txt")
	if err != nil || stat.Size != 5 || stat.IsDir {
		t.Fatalf("Stat 异常: %v %+v", err, stat)
	}
}

func TestConfigView(t *testing.T) {
	h := New(Options{WorkDir: t.TempDir(), Settings: map[string]any{
		"s": "text", "b": true, "i": 7, "f": float64(3),
	}})
	cfg := h.Config()
	if cfg.String("s", "def") != "text" || cfg.String("missing", "def") != "def" {
		t.Fatal("String 读取错误")
	}
	if !cfg.Bool("b", false) || !cfg.Bool("missing", true) {
		t.Fatal("Bool 读取错误")
	}
	if cfg.Int("i", 0) != 7 || cfg.Int("f", 0) != 3 || cfg.Int("missing", 9) != 9 {
		t.Fatal("Int 读取错误")
	}
	if raw, ok := cfg.Get("s"); !ok || string(raw) != `"text"` {
		t.Fatalf("Get 应返回 JSON，得到 %s %v", raw, ok)
	}
	if len(cfg.Snapshot()) != 4 {
		t.Fatal("Snapshot 条目数错误")
	}
}

func TestHostProtocolPlaceholders(t *testing.T) {
	h := New(Options{WorkDir: t.TempDir()})
	ctx := context.Background()

	if _, err := h.Spawn().Spawn(ctx, toolapi.SpawnRequest{}); !toolapi.IsNotImplemented(err) {
		t.Fatalf("Host.Spawn 应为协议占位，得到 %v", err)
	}
	if _, err := h.Tasks().Start(ctx, toolapi.TaskSpec{}); !toolapi.IsNotImplemented(err) {
		t.Fatalf("Host.Tasks 应为协议占位，得到 %v", err)
	}
	if _, err := h.Snapshots().Create(ctx, []string{"a"}); !toolapi.IsNotImplemented(err) {
		t.Fatalf("Host.Snapshots 应为协议占位，得到 %v", err)
	}
	if _, err := h.Exec().Run(ctx, toolapi.CommandRequest{Cmd: "echo"}); !toolapi.IsNotImplemented(err) {
		t.Fatalf("未接线的 Exec 应返回未实现，得到 %v", err)
	}
	if _, err := h.LLM().Chat(ctx, toolapi.LLMRequest{}); !toolapi.IsNotImplemented(err) {
		t.Fatalf("未接线的 LLM 应返回未实现，得到 %v", err)
	}
	if _, err := h.Blobs().Put(ctx, "text/plain", nil); !toolapi.IsNotImplemented(err) {
		t.Fatalf("未接线的 Blobs 应返回未实现，得到 %v", err)
	}
	if err := h.Events().Publish(ctx, "r1", "tool.started", nil); !toolapi.IsNotImplemented(err) {
		t.Fatalf("未接线的 Events 应返回未实现，得到 %v", err)
	}
}

func TestNewDefaultsWorkDir(t *testing.T) {
	h := New(Options{})
	wd, _ := os.Getwd()
	if filepath.Base(h.Workspace().Root()) != filepath.Base(wd) {
		t.Fatalf("默认工作目录应为进程 cwd，得到 %s", h.Workspace().Root())
	}
}
