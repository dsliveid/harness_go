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
	// Tasks 长任务状态机（G：long-task；SetTasks 注入）。
	Tasks toolapi.TaskManager
	// Snapshots 快照存储（G：fs 写入前/后登记；SetSnapshots 注入）。
	Snapshots toolapi.SnapshotStore
	// Bundles 会话导出包（G：session-bundle；SetBundles 注入）。
	Bundles toolapi.BundleStore
	// Images 图像能力（D：generate_image / recognize_image；SetImages 注入）。
	// 为可选能力：未注入时 Images() 返回 nil，插件据此返回「图像能力未启用」的诚实错误。
	Images toolapi.ImageProvider
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

// Images 可选图像能力（阶段 D：generate_image / recognize_image）。
// 未注入时返回 nil —— 这是**诚实的缺失信号**，插件据此返回「图像能力未启用」。
// 注意：本方法不在 toolapi.Host 接口内，通过 toolapi.ImageHost 类型断言获取。
func (h *Host) Images() toolapi.ImageProvider { return h.opts.Images }

// SetImages 后置注入图像能力（阶段 D）。
func (h *Host) SetImages(p toolapi.ImageProvider) { h.opts.Images = p }

// Tasks 实现 toolapi.Host（阶段 G 填实；未注入时诚实报错）。
func (h *Host) Tasks() toolapi.TaskManager {
	if h.opts.Tasks == nil {
		return taskManagerStub{}
	}
	return h.opts.Tasks
}

// SetTasks 后置注入长任务状态机（阶段 G）。
func (h *Host) SetTasks(m toolapi.TaskManager) { h.opts.Tasks = m }

// Snapshots 实现 toolapi.Host（阶段 G 填实；未注入时诚实报错）。
func (h *Host) Snapshots() toolapi.SnapshotStore {
	if h.opts.Snapshots == nil {
		return snapshotStoreStub{}
	}
	return h.opts.Snapshots
}

// SetSnapshots 后置注入快照存储（阶段 G）。
func (h *Host) SetSnapshots(st toolapi.SnapshotStore) { h.opts.Snapshots = st }

// Bundles 实现 toolapi.Host（阶段 G 填实；未注入时诚实报错）。
func (h *Host) Bundles() toolapi.BundleStore {
	if h.opts.Bundles == nil {
		return bundlesStub{}
	}
	return h.opts.Bundles
}

// SetBundles 后置注入会话导出包服务（阶段 G）。
func (h *Host) SetBundles(b toolapi.BundleStore) { h.opts.Bundles = b }

// ============ Workspace（沙箱实现） ============

// workspace 受沙箱约束的文件访问实现。
//
// 双根设计（2026-11 目录选择放开）：
//   - root：**进程级默认根**（main.go 注入的启动目录）。无 ctx 根时一律用它，
//     保证所有既有调用点（未接会话上下文的测试、后台任务）行为完全不变；
//   - ctx 根：由 tools.Runner 按会话 project_dir 注入（toolapi.WithWorkspaceRoot）。
//     带 ctx 的方法优先采用它，从而让「用户选的目录」真正成为执行根。
//
// 🚨 安全铁律：无论用哪个根，绝对路径拒绝 + `..` 上跳拒绝 + 前缀归属校验三道
// 防护必须原样保留。ctx 根只改变沙箱基准，绝不扩大授权范围。
type workspace struct{ root string }

// Root 实现 toolapi.Workspace。
//
// 注意：本方法无 ctx 参数，只能返回**进程级默认根**。需要会话根的场景请用
// RootFor(ctx)（见 ContextWorkspace 接口），fs 插件的 glob/grep 已按此切换。
func (w *workspace) Root() string { return w.root }

// RootFor 返回本次调用实际生效的执行根（ctx 有会话根则用它，否则回落进程根）。
func (w *workspace) RootFor(ctx context.Context) string {
	if r, ok := toolapi.WorkspaceRootFrom(ctx); ok {
		return r
	}
	return w.root
}

// Resolve 实现 toolapi.Workspace：拒绝绝对路径与越界路径。
func (w *workspace) Resolve(rel string) (string, error) {
	return w.resolveIn(w.root, rel)
}

// ResolveFor 同 Resolve，但按 ctx 会话根解析。
func (w *workspace) ResolveFor(ctx context.Context, rel string) (string, error) {
	return w.resolveIn(w.RootFor(ctx), rel)
}

// resolveIn 在有根 base 下解析 rel（三道沙箱防护的唯一实现）。
func (w *workspace) resolveIn(base, rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" || rel == "." {
		return base, nil
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: 不允许绝对路径 %q", toolapi.ErrInvalidArgs, rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: 路径越界 %q", toolapi.ErrInvalidArgs, rel)
	}
	abs := filepath.Join(base, clean)
	baseWithSep := base + string(filepath.Separator)
	if abs != base && !strings.HasPrefix(abs, baseWithSep) {
		return "", fmt.Errorf("%w: 路径越界 %q", toolapi.ErrInvalidArgs, rel)
	}
	return abs, nil
}

// ReadFile 实现 toolapi.Workspace。
func (w *workspace) ReadFile(ctx context.Context, rel string) ([]byte, error) {
	abs, err := w.ResolveFor(ctx, rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(abs)
}

// WriteFile 实现 toolapi.Workspace。
func (w *workspace) WriteFile(ctx context.Context, rel string, data []byte, perm fs.FileMode) error {
	abs, err := w.ResolveFor(ctx, rel)
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
func (w *workspace) List(ctx context.Context, rel string) ([]toolapi.Entry, error) {
	abs, err := w.ResolveFor(ctx, rel)
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
func (w *workspace) Stat(ctx context.Context, rel string) (toolapi.Entry, error) {
	abs, err := w.ResolveFor(ctx, rel)
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
func (w *workspace) Remove(ctx context.Context, rel string) error {
	base := w.RootFor(ctx)
	abs, err := w.resolveIn(base, rel)
	if err != nil {
		return err
	}
	if abs == base {
		return fmt.Errorf("%w: 不允许删除工作区根", toolapi.ErrInvalidArgs)
	}
	return os.RemoveAll(abs)
}

// Exists 实现 toolapi.Workspace。
func (w *workspace) Exists(ctx context.Context, rel string) (bool, error) {
	abs, err := w.ResolveFor(ctx, rel)
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

// 编译期断言：支持 ctx 感知的执行根（供 fs 插件 glob/grep 使用）。
var _ toolapi.ContextWorkspace = (*workspace)(nil)

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

type bundlesStub struct{}

func (bundlesStub) Export(context.Context, string) (toolapi.BundleInfo, error) {
	return toolapi.BundleInfo{}, fmt.Errorf("%w: 会话导出（Host.Bundles）阶段 G 填实", toolapi.ErrNotImplemented)
}

func (bundlesStub) Import(context.Context, string) (toolapi.BundleInfo, error) {
	return toolapi.BundleInfo{}, fmt.Errorf("%w: 会话导入（Host.Bundles）阶段 G 填实", toolapi.ErrNotImplemented)
}

func (bundlesStub) List(context.Context, string) ([]toolapi.BundleInfo, error) {
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
