package toolapi

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"time"
)

// Host 内核注入给插件的**受控能力集合**（共 9 项，D13 定稿）。
//
// 铁律：插件拿不到 *sql.DB、拿不到原始 llm.Provider、拿不到 os 级任意文件访问；
// 一切能力都必须经此接口，从而统一施加沙箱、审批与配额约束。
//
// A 阶段 Spawn/Tasks/Snapshots 返回 ErrNotImplemented 的桩实现（协议占位，阶段 F/G 填实）。
type Host interface {
	// Workspace 受沙箱约束的工作区文件访问（根固定为项目级，不做会话级动态根）。
	Workspace() Workspace
	// Exec 受审批约束的子进程执行。
	Exec() CommandRunner
	// LLM 受配额约束的模型调用。
	LLM() LLM
	// Blobs 会话级大对象存储（CAS）。
	Blobs() Blobs
	// Events 向当前 Run 的事件流发布里程碑事件。
	Events() Events
	// Config 只读配置视图。
	Config() Config
	// Spawn 派生嵌套 Run（subagent / collaborator），受 max_depth 与配额约束。
	Spawn() ChildRunner
	// Tasks 长任务状态机（start/pause/resume/cancel/checkpoint）。
	Tasks() TaskManager
	// Snapshots 文件快照与回滚引用。
	Snapshots() SnapshotStore
}

// Entry 工作区条目。
type Entry struct {
	Path    string      `json:"path"`
	IsDir   bool        `json:"is_dir"`
	Size    int64       `json:"size"`
	ModTime time.Time   `json:"mod_time"`
	Mode    fs.FileMode `json:"mode"`
}

// Workspace 工作区文件访问（所有路径均为相对工作区根，越界即拒绝）。
type Workspace interface {
	// Root 返回工作区绝对根路径（只读，插件不得据此绕过沙箱）。
	Root() string
	// Resolve 把相对路径解析为绝对路径，越界返回错误。
	Resolve(rel string) (string, error)
	// ReadFile 读取文件。
	ReadFile(ctx context.Context, rel string) ([]byte, error)
	// WriteFile 写入文件（父目录不存在时创建）。
	WriteFile(ctx context.Context, rel string, data []byte, perm fs.FileMode) error
	// List 列出目录条目。
	List(ctx context.Context, rel string) ([]Entry, error)
	// Stat 查看条目信息。
	Stat(ctx context.Context, rel string) (Entry, error)
	// Remove 删除文件或目录。
	Remove(ctx context.Context, rel string) error
	// Exists 判断路径是否存在。
	Exists(ctx context.Context, rel string) (bool, error)
}

// CommandRequest 命令执行请求。
type CommandRequest struct {
	Cmd     string            `json:"cmd"`
	Args    []string          `json:"args,omitempty"`
	Dir     string            `json:"dir,omitempty"` // 相对工作区根
	Env     map[string]string `json:"env,omitempty"`
	Timeout time.Duration     `json:"timeout,omitempty"`
}

// CommandResult 命令执行结果。
type CommandResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	TimedOut bool   `json:"timed_out,omitempty"`
}

// CommandRunner 子进程执行（必须经审批闸门，危险命令需确认）。
type CommandRunner interface {
	Run(ctx context.Context, req CommandRequest) (CommandResult, error)
}

// LLMMessage 简化消息（插件层不依赖内核 llm 包）。
type LLMMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// LLMRequest 模型调用请求。
type LLMRequest struct {
	ModelRef    string       `json:"model_ref"`
	System      string       `json:"system,omitempty"`
	Messages    []LLMMessage `json:"messages"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`
}

// TokenUsage 用量。
type TokenUsage struct {
	PromptTokens     int  `json:"prompt_tokens"`
	CompletionTokens int  `json:"completion_tokens"`
	TotalTokens      int  `json:"total_tokens"`
	Estimated        bool `json:"estimated"`
}

// LLMResponse 模型回复。
type LLMResponse struct {
	Content string     `json:"content"`
	Usage   TokenUsage `json:"usage"`
}

// LLM 模型调用（受 provider 配额与并发约束）。
type LLM interface {
	Chat(ctx context.Context, req LLMRequest) (LLMResponse, error)
}

// BlobRef 大对象引用。
type BlobRef struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
	MIME string `json:"mime"`
}

// BlobInfo 大对象元信息。
type BlobInfo struct {
	BlobRef
	CreatedAt time.Time `json:"created_at"`
}

// Blobs 会话级 CAS 存储（单会话内去重，删会话即回收）。
type Blobs interface {
	Put(ctx context.Context, mime string, r io.Reader) (BlobRef, error)
	Open(ctx context.Context, hash string) (io.ReadCloser, error)
	Stat(ctx context.Context, hash string) (BlobInfo, error)
}

// Events 事件发布（允许的 type 见 docs/protocols.md §2）。
type Events interface {
	Publish(ctx context.Context, runID string, typ string, payload any) error
}

// Config 只读配置视图（写配置必须走 HTTP API，避免多实例不一致）。
type Config interface {
	Get(key string) (json.RawMessage, bool)
	String(key, def string) string
	Bool(key string, def bool) bool
	Int(key string, def int) int
	Snapshot() map[string]any
}

// ChildStatus 子 Run 状态（**必须显式返回终态**，禁止用「没在跑」推断成功）。
type ChildStatus string

const (
	// ChildPending 排队中。
	ChildPending ChildStatus = "pending"
	// ChildRunning 执行中。
	ChildRunning ChildStatus = "running"
	// ChildWaitingApproval 等待审批。
	ChildWaitingApproval ChildStatus = "waiting_approval"
	// ChildCompleted 成功终态。
	ChildCompleted ChildStatus = "completed"
	// ChildFailed 失败终态。
	ChildFailed ChildStatus = "failed"
	// ChildCancelled 取消终态。
	ChildCancelled ChildStatus = "cancelled"
	// ChildInterrupted 进程重启导致的终态。
	ChildInterrupted ChildStatus = "interrupted"
)

// Terminal 判断是否为终态。
func (s ChildStatus) Terminal() bool {
	switch s {
	case ChildCompleted, ChildFailed, ChildCancelled, ChildInterrupted:
		return true
	}
	return false
}

// SpawnRequest 派生嵌套 Run 的请求。
type SpawnRequest struct {
	ParentRunID string         `json:"parent_run_id"`
	SessionID   string         `json:"session_id"`
	AgentRole   string         `json:"agent_role"`
	Input       string         `json:"input"`
	ModelRef    string         `json:"model_ref,omitempty"`
	MaxSteps    int            `json:"max_steps,omitempty"`
	Meta        map[string]any `json:"meta,omitempty"`
}

// ChildResult 子 Run 结果。
type ChildResult struct {
	RunID  string      `json:"run_id"`
	Status ChildStatus `json:"status"`
	Output string      `json:"output,omitempty"`
	Error  string      `json:"error,omitempty"`
	Usage  TokenUsage  `json:"usage"`
}

// ChildRun 子 Run 句柄。
type ChildRun interface {
	ID() string
	Status() ChildStatus
	Wait(ctx context.Context) (ChildResult, error)
	Cancel() error
}

// ChildRunner 嵌套 Run 派生器（禁递归：max_depth 由内核强制）。
type ChildRunner interface {
	Spawn(ctx context.Context, req SpawnRequest) (ChildRun, error)
	Get(runID string) (ChildRun, bool)
}

// TaskState 长任务状态。
type TaskState string

const (
	// TaskPending 待开始。
	TaskPending TaskState = "pending"
	// TaskRunning 执行中。
	TaskRunning TaskState = "running"
	// TaskPaused 已暂停。
	TaskPaused TaskState = "paused"
	// TaskCompleted 成功终态。
	TaskCompleted TaskState = "completed"
	// TaskFailed 失败终态。
	TaskFailed TaskState = "failed"
	// TaskCancelled 取消终态。
	TaskCancelled TaskState = "cancelled"
)

// TaskStep 长任务步骤。
type TaskStep struct {
	ID     string    `json:"id"`
	Title  string    `json:"title"`
	Status TaskState `json:"status"`
	Note   string    `json:"note,omitempty"`
}

// TaskSpec 长任务定义。
type TaskSpec struct {
	ID        string         `json:"id"`
	RunID     string         `json:"run_id"`
	SessionID string         `json:"session_id"`
	Title     string         `json:"title"`
	Steps     []TaskStep     `json:"steps"`
	Meta      map[string]any `json:"meta,omitempty"`
}

// TaskCheckpoint 检查点（Run + 快照引用的原子登记）。
type TaskCheckpoint struct {
	RunID      string    `json:"run_id"`
	SnapshotID string    `json:"snapshot_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// Task 长任务句柄。
type Task interface {
	ID() string
	State() TaskState
	Steps() []TaskStep
	Pause() error
	Resume() error
	Cancel() error
}

// TaskManager 长任务状态机（阶段 G 填实）。
type TaskManager interface {
	Start(ctx context.Context, spec TaskSpec) (Task, error)
	Get(id string) (Task, bool)
	List() []Task
	Checkpoint(ctx context.Context, id string) (TaskCheckpoint, error)
}

// SnapshotRef 快照引用（全局 CAS，跨会话去重）。
type SnapshotRef struct {
	ID        string    `json:"id"`
	Paths     []string  `json:"paths"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

// SnapshotStore 文件快照与回滚（阶段 G 填实）。
type SnapshotStore interface {
	Create(ctx context.Context, paths []string) (SnapshotRef, error)
	Restore(ctx context.Context, ref SnapshotRef) error
	List(ctx context.Context) ([]SnapshotRef, error)
}
