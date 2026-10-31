// Package plugintest 提供插件测试的假 Host（真实文件系统工作区 + 可注入桩）。
//
// 只用于测试装配（非运行时组件）：Workspace 直连临时目录——测试关注工具语义，
// 沙箱行为由 hostimpl 自己的测试覆盖。
package plugintest

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"harness-go/pkg/toolapi"
)

// Host 插件测试假 Host。
type Host struct {
	dir string
	mu  sync.Mutex
	// ExecFn 可注入的命令执行桩（nil = 未接线错误）。
	ExecFn func(ctx context.Context, req toolapi.CommandRequest) (toolapi.CommandResult, error)
	// SpawnFn 可注入的嵌套 Run 桩（nil = 未接线错误）。
	SpawnFn func(ctx context.Context, req toolapi.SpawnRequest) (toolapi.ChildRun, error)
	// TaskFn 可注入的长任务状态机（nil = 内存桩）。
	TaskFn func() toolapi.TaskManager
	// SnapshotFn 可注入的快照存储（nil = tempdir 文件桩）。
	SnapshotFn func() toolapi.SnapshotStore
	// BundleFn 可注入的会话导出包（nil = 内存记录桩）。
	BundleFn func() toolapi.BundleStore
	// ProgressCh 收集 tool.progress 推送（nil = 不收集）。
	ProgressCh chan string

	// —— 阶段 G 桩（懒初始化，跨工具调用保持状态） ——
	snap   *snapStub
	task   *taskStub
	bundle *bundleStub
}

// New 创建假 Host（工作区 = 临时目录）。
func New(t interface{ TempDir() string }) *Host {
	return &Host{dir: t.TempDir()}
}

// Dir 返回工作区根。
func (h *Host) Dir() string { return h.dir }

// Seed 在工作区写入测试文件。
func (h *Host) Seed(rel, content string) error {
	abs := filepath.Join(h.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, []byte(content), 0o644)
}

// ---------- toolapi.Host ----------

// Workspace 实现 toolapi.Host。
func (h *Host) Workspace() toolapi.Workspace { return h }

// Exec 实现 toolapi.Host。
func (h *Host) Exec() toolapi.CommandRunner { return execAdapter{h} }

// LLM 实现 toolapi.Host（F 测试不触真实模型）。
func (h *Host) LLM() toolapi.LLM { return nil }

// Blobs 实现 toolapi.Host（F 测试用不到）。
func (h *Host) Blobs() toolapi.Blobs { return nil }

// Events 实现 toolapi.Host。
func (h *Host) Events() toolapi.Events { return h }

// Config 实现 toolapi.Host（空配置）。
func (h *Host) Config() toolapi.Config { return emptyConfig{} }

// Spawn 实现 toolapi.Host。
func (h *Host) Spawn() toolapi.ChildRunner { return spawnAdapter{h} }

// Tasks 实现 toolapi.Host（内存桩：测试可用 TaskFn 覆写）。
func (h *Host) Tasks() toolapi.TaskManager {
	if h.TaskFn != nil {
		return h.TaskFn()
	}
	return h.taskStub()
}

// Snapshots 实现 toolapi.Host（文件桩：tempdir CAS + 内存索引，
// 语义与 internal/snapshot 一致——fs 插件的快照登记测试在真实文件上跑）。
func (h *Host) Snapshots() toolapi.SnapshotStore {
	if h.SnapshotFn != nil {
		return h.SnapshotFn()
	}
	return h.snapshotStub()
}

// Bundles 实现 toolapi.Host（内存记录桩：测试可用 BundleFn 覆写）。
func (h *Host) Bundles() toolapi.BundleStore {
	if h.BundleFn != nil {
		return h.BundleFn()
	}
	return h.bundleStub()
}

// ---------- toolapi.Workspace ----------

// Root 实现 toolapi.Workspace。
func (h *Host) Root() string { return h.dir }

// Resolve 实现 toolapi.Workspace（越界拒绝，与 hostimpl 同口径）。
func (h *Host) Resolve(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" || rel == "." {
		return h.dir, nil
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(filepath.Clean(rel), "..") {
		return "", fmt.Errorf("越界: %q", rel)
	}
	return filepath.Join(h.dir, filepath.FromSlash(rel)), nil
}

// ReadFile 实现 toolapi.Workspace。
func (h *Host) ReadFile(_ context.Context, rel string) ([]byte, error) {
	abs, err := h.Resolve(rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(abs)
}

// WriteFile 实现 toolapi.Workspace。
func (h *Host) WriteFile(_ context.Context, rel string, data []byte, perm fs.FileMode) error {
	abs, err := h.Resolve(rel)
	if err != nil {
		return err
	}
	if perm == 0 {
		perm = 0o644
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, data, perm)
}

// List 实现 toolapi.Workspace。
func (h *Host) List(_ context.Context, rel string) ([]toolapi.Entry, error) {
	abs, err := h.Resolve(rel)
	if err != nil {
		return nil, err
	}
	items, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	out := make([]toolapi.Entry, 0, len(items))
	for _, it := range items {
		info, iErr := it.Info()
		if iErr != nil {
			continue
		}
		p := it.Name()
		if rel != "." && rel != "" {
			p = strings.TrimPrefix(filepath.ToSlash(filepath.Join(filepath.ToSlash(rel), it.Name())), "./")
		}
		out = append(out, toolapi.Entry{Path: p, IsDir: it.IsDir(), Size: info.Size(), ModTime: info.ModTime()})
	}
	return out, nil
}

// Stat 实现 toolapi.Workspace。
func (h *Host) Stat(_ context.Context, rel string) (toolapi.Entry, error) {
	abs, err := h.Resolve(rel)
	if err != nil {
		return toolapi.Entry{}, err
	}
	info, serr := os.Stat(abs)
	if serr != nil {
		return toolapi.Entry{}, serr
	}
	return toolapi.Entry{
		Path:    filepath.ToSlash(filepath.Clean(rel)),
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}, nil
}

// Remove 实现 toolapi.Workspace。
func (h *Host) Remove(_ context.Context, rel string) error {
	abs, err := h.Resolve(rel)
	if err != nil {
		return err
	}
	return os.RemoveAll(abs)
}

// Exists 实现 toolapi.Workspace。
func (h *Host) Exists(_ context.Context, rel string) (bool, error) {
	abs, err := h.Resolve(rel)
	if err != nil {
		return false, err
	}
	_, serr := os.Stat(abs)
	return serr == nil, nil
}

// ---------- toolapi.Events ----------

// Publish 实现 toolapi.Events（tool.progress 收集到 ProgressCh）。
func (h *Host) Publish(_ context.Context, _ string, typ string, payload any) error {
	if typ == "tool.progress" && h.ProgressCh != nil {
		if m, ok := payload.(map[string]any); ok {
			if chunk, ok := m["chunk"].(string); ok {
				h.mu.Lock()
				select {
				case h.ProgressCh <- chunk:
				default:
				}
				h.mu.Unlock()
			}
		}
	}
	return nil
}

// ---------- 适配器 ----------

type execAdapter struct{ h *Host }

func (a execAdapter) Run(ctx context.Context, req toolapi.CommandRequest) (toolapi.CommandResult, error) {
	if a.h.ExecFn == nil {
		return toolapi.CommandResult{}, fmt.Errorf("命令执行未接线")
	}
	return a.h.ExecFn(ctx, req)
}

type spawnAdapter struct{ h *Host }

func (a spawnAdapter) Spawn(ctx context.Context, req toolapi.SpawnRequest) (toolapi.ChildRun, error) {
	if a.h.SpawnFn == nil {
		return nil, fmt.Errorf("Spawn 未注入")
	}
	return a.h.SpawnFn(ctx, req)
}

func (spawnAdapter) Get(string) (toolapi.ChildRun, bool) { return nil, false }

type emptyConfig struct{}

func (emptyConfig) Get(string) (json.RawMessage, bool) { return nil, false }
func (emptyConfig) String(string, string) string       { return "" }
func (emptyConfig) Bool(string, bool) bool             { return false }
func (emptyConfig) Int(string, int) int                { return 0 }
func (emptyConfig) Snapshot() map[string]any           { return nil }

var _ = time.Now
