// Package model 定义跨层共享的纯数据模型（DTO），零业务依赖。
//
// 为什么要独立一层：架构要求依赖单向向内（api → service → agent → 端口适配器），
// 若把模型定义在 store 包，agent 就会被追依赖适配器。放在 model 后，
// store 与 agent 都只依赖 model，依赖方向保持干净。
package model

import "encoding/json"

// RunStatus Run 状态机取值（docs/架构设计文档.md §7.2）。
type RunStatus string

const (
	RunPending         RunStatus = "pending"
	RunRunning         RunStatus = "running"
	RunWaitingApproval RunStatus = "waiting_approval"
	RunCompleted       RunStatus = "completed"
	RunFailed          RunStatus = "failed"
	RunCancelled       RunStatus = "cancelled"
	RunInterrupted     RunStatus = "interrupted"
)

// IsTerminal 是否终态。
func (s RunStatus) IsTerminal() bool {
	switch s {
	case RunCompleted, RunFailed, RunCancelled, RunInterrupted:
		return true
	}
	return false
}

// IsUnfinished 是否需要进程重启后置为 interrupted。
func (s RunStatus) IsUnfinished() bool {
	switch s {
	case RunPending, RunRunning, RunWaitingApproval:
		return true
	}
	return false
}

// Valid 是否为合法状态值。
func (s RunStatus) Valid() bool {
	switch s {
	case RunPending, RunRunning, RunWaitingApproval,
		RunCompleted, RunFailed, RunCancelled, RunInterrupted:
		return true
	}
	return false
}

// Role 消息角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Executor 执行面归属（多端协同 HALO）。
type Executor string

const (
	ExecutorServer  Executor = "server"
	ExecutorDesktop Executor = "desktop"
	ExecutorMobile  Executor = "mobile"
	ExecutorBrowser Executor = "browser"
	ExecutorAny     Executor = "any"
)

// TrustLevel 结果来源可信度（端侧结果一律 untrusted）。
type TrustLevel string

const (
	TrustTrusted   TrustLevel = "trusted"
	TrustUntrusted TrustLevel = "untrusted"
)

// ToolEventStatus 工具调用状态。
type ToolEventStatus string

const (
	ToolPending  ToolEventStatus = "pending"
	ToolApproved ToolEventStatus = "approved"
	ToolDenied   ToolEventStatus = "denied"
	ToolRunning  ToolEventStatus = "running"
	ToolOK       ToolEventStatus = "ok"
	ToolError    ToolEventStatus = "error"
	ToolTimeout  ToolEventStatus = "timeout"
)

// Risk 工具风险级。
type Risk string

const (
	RiskReadonly Risk = "readonly"
	RiskWrite    Risk = "write"
	RiskCommand  Risk = "command"
	RiskDanger   Risk = "danger"
)

// Session 会话（主库）。
type Session struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	WorkspacePath string    `json:"workspace_path"`
	AccessMode    string    `json:"access_mode"`
	ModelRef      string    `json:"model_ref"`
	UserID        string    `json:"user_id"`
	MessageCount  int64     `json:"message_count"`
	LastMessageAt *int64    `json:"last_message_at,omitempty"`
	TotalTokens   int64     `json:"total_tokens"`
	LastRunStatus RunStatus `json:"last_run_status,omitempty"`
	CreatedAt     int64     `json:"created_at"`
	UpdatedAt     int64     `json:"updated_at"`
	DeletedAt     *int64    `json:"deleted_at,omitempty"`
}

// Message 消息（会话库）。
type Message struct {
	ID              string          `json:"id"`
	Seq             int64           `json:"seq"`
	RunID           string          `json:"run_id,omitempty"`
	Role            Role            `json:"role"`
	Content         string          `json:"content"`
	Reasoning       string          `json:"reasoning,omitempty"`
	ToolCallsJSON   json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID      string          `json:"tool_call_id,omitempty"`
	AttachmentsJSON json.RawMessage `json:"attachments,omitempty"`
	TokenCount      int             `json:"token_count"`
	UsageJSON       json.RawMessage `json:"usage,omitempty"`
	CreatedAt       int64           `json:"created_at"`
}

// Run 运行（会话库）。
type Run struct {
	ID           string    `json:"id"`
	TriggerType  string    `json:"trigger_type"`
	Status       RunStatus `json:"status"`
	StopReason   string    `json:"stop_reason,omitempty"`
	ModelRef     string    `json:"model_ref"`
	Executor     Executor  `json:"executor"`
	DeviceID     string    `json:"device_id,omitempty"`
	ParentRunID  string    `json:"parent_run_id,omitempty"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	CachedTokens int64     `json:"cached_tokens"`
	IsEstimated  bool      `json:"is_estimated"`
	ErrorCode    string    `json:"error_code,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
	StartedAt    *int64    `json:"started_at,omitempty"`
	EndedAt      *int64    `json:"ended_at,omitempty"`
	CreatedAt    int64     `json:"created_at"`
}

// User 本地账号（主库）。
type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	DisplayName  string `json:"display_name"`
	Role         string `json:"role"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

// Token 令牌（主库）。明文永不入库，只存 sha256。
type Token struct {
	ID         string `json:"id"`
	UserID     string `json:"user_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"` // api | web_session | sso
	TokenHash  string `json:"-"`
	Scope      string `json:"scope"` // read | write | admin
	ExpiresAt  *int64 `json:"expires_at,omitempty"`
	LastUsedAt *int64 `json:"last_used_at,omitempty"`
	RevokedAt  *int64 `json:"revoked_at,omitempty"`
	CreatedAt  int64  `json:"created_at"`
}

// TokenKind 令牌类型常量。
const (
	TokenKindAPI        = "api"
	TokenKindWebSession = "web_session"
	TokenKindSSO        = "sso"
)

// ApprovalRule 会话级审批规则（由「本会话允许」决策产生）。
type ApprovalRule struct {
	ID        string `json:"id"`
	ToolName  string `json:"tool_name"`
	Pattern   string `json:"pattern"`
	CreatedAt int64  `json:"created_at"`
}

// ToolEvent 工具调用事件（会话库）。
type ToolEvent struct {
	ID          string          `json:"id"`
	RunID       string          `json:"run_id"`
	MessageID   string          `json:"message_id,omitempty"`
	ToolName    string          `json:"tool_name"`
	Namespace   string          `json:"namespace"`
	ArgsJSON    json.RawMessage `json:"args,omitempty"`
	ResultJSON  json.RawMessage `json:"result,omitempty"`
	ResultBlob  string          `json:"result_blob,omitempty"`
	Status      ToolEventStatus `json:"status"`
	Risk        Risk            `json:"risk"`
	Executor    Executor        `json:"executor"`
	DeviceID    string          `json:"device_id,omitempty"`
	SourceTrust TrustLevel      `json:"source_trust"`
	ApprovedBy  string          `json:"approved_by,omitempty"`
	Error       string          `json:"error,omitempty"`
	StartedAt   *int64          `json:"started_at,omitempty"`
	EndedAt     *int64          `json:"ended_at,omitempty"`
}
