// Package hostimpl 提供 toolapi.Host 的内核实现（插件唯一受控能力入口）。
//
// A 阶段实现策略：
//   - Workspace / Config / Events 完整实现（沙箱与只读视图）；
//   - Exec / LLM / Blobs 由 main 注入具体实现，未注入时返回 ErrNotImplemented；
//   - Spawn / Tasks / Snapshots 为协议占位桩（阶段 F/G 填实），调用即返回 ErrNotImplemented。
package hostimpl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"harness-go/pkg/toolapi"
)

// Options Host 依赖（nil 项表示未接线，调用时返回 ErrNotImplemented）。
type Options struct {
	// WorkDir 工作区绝对根路径（固定项目级，不做会话级动态根）。
	WorkDir string
	// Settings 只读配置快照（可为 nil）。
	Settings map[string]any
	// Commands 子进程执行（受审批约束）。
	Commands toolapi.CommandRunner
	// Models 模型调用（受配额约束）。
	Models toolapi.LLM
	// Blobs 会话级 CAS（需按会话注入，A 阶段由调用方决定）。
	Blobs toolapi.Blobs
	// Events 事件发布（per-Run）。
	Events toolapi.Events
	// ChildRunner 嵌套 Run 派生器（F：subagent/collaborator；后置注入打破构造环）。
	ChildRunner toolapi.ChildRunner
	// CommandEnvWhitelist 子进程继承的宿主环境变量键。
	CommandEnvWhitelist []string
}

// Host toolapi.Host 实现。
type Host struct {
	opts Options
	ws   *workspace
	cfg  *config
}

// New 创建 Host（工作目录为空时取进程当前目录）。
func New(opts Options) *Host {
	root := opts.WorkDir
	if root == "" {
		root, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &Host{
		opts: opts,
		ws:   &workspace{root: root},
		cfg:  &config{values: opts.Settings},
	}
}

// Workspace 实现 toolapi.Host。
func (h *Host) Workspace() toolapi.Workspace { return h.ws }

// Exec 实现 toolapi.Host。
func (h *Host) Exec() toolapi.CommandRunner {
	if h.opts.Commands == nil {
		return commandStub{}
	}
	return h.opts.Commands
}

// LLM 实现 toolapi.Host。
func (h *Host) LLM() toolapi.LLM {
	if h.opts.Models == nil {
		return llmStub{}
	}
	return h.opts.Models
}

// Blobs 实现 toolapi.Host。
func (h *Host) Blobs() toolapi.Blobs {
	if h.opts.Blobs == nil {
		return blobsStub{}
	}
	return h.opts.Blobs
}

// Events 实现 toolapi.Host。
func (h *Host) Events() toolapi.Events {
	if h.opts.Events == nil {
		return eventsStub{}
	}
	return h.opts.Events
}

// Config 实现 toolapi.Host。
func (h *Host) Config() toolapi.Config { return h.cfg }

// Spawn 实现 toolapi.ChildRunner（阶段 F 填实；未注入时诚实报错）。
func (h *Host) Spawn() toolapi.ChildRunner {
	if h.opts.ChildRunner == nil {
		return childRunnerStub{}
	}
	return h.opts.ChildRunner
}

// SetChildRunner 后置注入嵌套 Run 派生器（构造环：host → registry → engine → host）。
func (h *Host) SetChildRunner(cr toolapi.ChildRunner) { h.opts.ChildRunner = cr }

// SetCommandRunner 后置注入命令执行器（阶段 F）。
func (h *Host) SetCommandRunner(cr toolapi.CommandRunner) { h.opts.Commands = cr }

// SetLLM 后置注入模型调用（阶段 F）。
func (h *Host) SetLLM(l toolapi.LLM) { h.opts.Models = l }

// SetEvents 后置注入事件发布（阶段 F）。
func (h *Host) SetEvents(e toolapi.Events) { h.opts.Events = e }

// Tasks 实现 toolapi.Host（阶段 G 填实）。
func (h *Host) Tasks() toolapi.TaskManager { return taskManagerStub{} }

// Snapshots 实现 toolapi.Host（阶段 G 填实）。
func (h *Host) Snapshots() toolapi.SnapshotStore { return snapshotStoreStub{} }

// ============ Workspace（沙箱实现） ============

type workspace struct{ root string }

// Root 实现 toolapi.Workspace。
func (w *workspace) Root() string { return w.root }

// Resolve 实现 toolapi.Workspace：拒绝绝对路径与越界路径。
func (w *workspace) Resolve(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" || rel == "." {
		return w.root, nil
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: 不允许绝对路径 %q", toolapi.ErrInvalidArgs, rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: 路径越界 %q", toolapi.ErrInvalidArgs, rel)
	}
	abs := filepath.Join(w.root, clean)
	rootWithSep := w.root + string(filepath.Separator)
	if abs != w.root && !strings.HasPrefix(abs, rootWithSep) {
		return "", fmt.Errorf("%w: 路径越界 %q", toolapi.ErrInvalidArgs, rel)
	}
	return abs, nil
}

// ReadFile 实现 toolapi.Workspace。
func (w *workspace) ReadFile(_ context.Context, rel string) ([]byte, error) {
	abs, err := w.Resolve(rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(abs)
}

// WriteFile 实现 toolapi.Workspace。
func (w *workspace) WriteFile(_ context.Context, rel string, data []byte, perm fs.FileMode) error {
	abs, err := w.Resolve(rel)
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
func (w *workspace) List(_ context.Context, rel string) ([]toolapi.Entry, error) {
	abs, err := w.Resolve(rel)
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
		out = append(out, toolapi.Entry{
			Path:    filepath.ToSlash(filepath.Join(filepath.Clean(rel), it.Name())),
			IsDir:   it.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
			Mode:    info.Mode(),
		})
	}
	return out, nil
}

// Stat 实现 toolapi.Workspace。
func (w *workspace) Stat(_ context.Context, rel string) (toolapi.Entry, error) {
	abs, err := w.Resolve(rel)
	if err != nil {
		return toolapi.Entry{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return toolapi.Entry{}, err
	}
	return toolapi.Entry{
		Path:    filepath.ToSlash(filepath.Clean(rel)),
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		ModTime: info.ModTime(),
		Mode:    info.Mode(),
	}, nil
}

// Remove 实现 toolapi.Workspace。
func (w *workspace) Remove(_ context.Context, rel string) error {
	abs, err := w.Resolve(rel)
	if err != nil {
		return err
	}
	if abs == w.root {
		return fmt.Errorf("%w: 不允许删除工作区根", toolapi.ErrInvalidArgs)
	}
	return os.RemoveAll(abs)
}

// Exists 实现 toolapi.Workspace。
func (w *workspace) Exists(_ context.Context, rel string) (bool, error) {
	abs, err := w.Resolve(rel)
	if err != nil {
		return false, err
	}
	_, sErr := os.Stat(abs)
	if sErr == nil {
		return true, nil
	}
	if os.IsNotExist(sErr) {
		return false, nil
	}
	return false, sErr
}

// ============ Config（只读视图） ============

type config struct{ values map[string]any }

// Get 实现 toolapi.Config。
func (c *config) Get(key string) (json.RawMessage, bool) {
	v, ok := c.values[key]
	if !ok {
		return nil, false
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(b), true
}

// String 实现 toolapi.Config。
func (c *config) String(key, def string) string {
	if v, ok := c.values[key]; ok {
		if s, isStr := v.(string); isStr {
			return s
		}
		return fmt.Sprint(v)
	}
	return def
}

// Bool 实现 toolapi.Config。
func (c *config) Bool(key string, def bool) bool {
	switch v := c.values[key].(type) {
	case bool:
		return v
	case string:
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// Int 实现 toolapi.Config。
func (c *config) Int(key string, def int) int {
	switch v := c.values[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// Snapshot 实现 toolapi.Config。
func (c *config) Snapshot() map[string]any {
	out := make(map[string]any, len(c.values))
	for k, v := range c.values {
		out[k] = v
	}
	return out
}

// ============ 未接线 / 占位桩 ============

type commandStub struct{}

func (commandStub) Run(context.Context, toolapi.CommandRequest) (toolapi.CommandResult, error) {
	return toolapi.CommandResult{}, fmt.Errorf("%w: 子进程执行未接线（阶段 F）", toolapi.ErrNotImplemented)
}

type llmStub struct{}

func (llmStub) Chat(context.Context, toolapi.LLMRequest) (toolapi.LLMResponse, error) {
	return toolapi.LLMResponse{}, fmt.Errorf("%w: 模型调用未接线（阶段 F）", toolapi.ErrNotImplemented)
}

type blobsStub struct{}

func (blobsStub) Put(context.Context, string, io.Reader) (toolapi.BlobRef, error) {
	return toolapi.BlobRef{}, fmt.Errorf("%w: blob 存储未接线（阶段 F）", toolapi.ErrNotImplemented)
}

func (blobsStub) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("%w: blob 存储未接线（阶段 F）", toolapi.ErrNotImplemented)
}

func (blobsStub) Stat(context.Context, string) (toolapi.BlobInfo, error) {
	return toolapi.BlobInfo{}, fmt.Errorf("%w: blob 存储未接线（阶段 F）", toolapi.ErrNotImplemented)
}

type eventsStub struct{}

func (eventsStub) Publish(context.Context, string, string, any) error {
	return fmt.Errorf("%w: 事件发布未接线（阶段 F）", toolapi.ErrNotImplemented)
}

type childRunnerStub struct{}

func (childRunnerStub) Spawn(context.Context, toolapi.SpawnRequest) (toolapi.ChildRun, error) {
	return nil, fmt.Errorf("%w: 嵌套 Run 未接线（subagent 不可用）", toolapi.ErrNotImplemented)
}

func (childRunnerStub) Get(string) (toolapi.ChildRun, bool) { return nil, false }

type taskManagerStub struct{}

func (taskManagerStub) Start(context.Context, toolapi.TaskSpec) (toolapi.Task, error) {
	return nil, fmt.Errorf("%w: 长任务（Host.Tasks）阶段 G 填实", toolapi.ErrNotImplemented)
}

func (taskManagerStub) Get(string) (toolapi.Task, bool) { return nil, false }

func (taskManagerStub) List() []toolapi.Task { return nil }

func (taskManagerStub) Checkpoint(context.Context, string) (toolapi.TaskCheckpoint, error) {
	return toolapi.TaskCheckpoint{}, fmt.Errorf("%w: 长任务检查点（Host.Tasks）阶段 G 填实", toolapi.ErrNotImplemented)
}

type snapshotStoreStub struct{}

func (snapshotStoreStub) Create(context.Context, []string) (toolapi.SnapshotRef, error) {
	return toolapi.SnapshotRef{}, fmt.Errorf("%w: 快照（Host.Snapshots）阶段 G 填实", toolapi.ErrNotImplemented)
}

func (snapshotStoreStub) Restore(context.Context, toolapi.SnapshotRef) error {
	return fmt.Errorf("%w: 快照回滚（Host.Snapshots）阶段 G 填实", toolapi.ErrNotImplemented)
}

func (snapshotStoreStub) List(context.Context) ([]toolapi.SnapshotRef, error) {
	return nil, nil
}

// 保证编译期接口对齐（防止契约漂移）。
var (
	_ toolapi.Host      = (*Host)(nil)
	_ toolapi.Workspace = (*workspace)(nil)
	_ toolapi.Config    = (*config)(nil)
)

// 避免未使用导入（time 供 BlobInfo / Entry 使用已在其他文件）。
var _ = time.Time{}
