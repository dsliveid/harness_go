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
	ExecutorDevice  Executor = "device"
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
//
// ⚠️ 三个归属字段极易混淆，权威定义见 docs/术语表-工作空间与项目目录.md：
//   - WorkspaceID 工作空间（执行面，对应一台设备）——决定能力天花板
//   - ProjectID   项目（可选）——纯对话则无
//   - ProjectDir  【项目目录】（磁盘落盘路径）——决定工具在哪个目录执行
//
// 📌 历史说明：ProjectDir 原名 WorkspacePath。改名原因是它与 WorkspaceID 都带
// Workspace 前缀却语义无关，曾导致「清空 workspace_path」被误读为「清空工作空间」
// 并推出错误需求结论。DB 层由 0013 迁移同步更名。
type Session struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// ProjectDir 是**项目目录**（磁盘落盘路径），不是工作空间。
	// 决定工具在哪个目录下执行；与 WorkspaceID（执行面归属）互相独立。
	// 原名 WorkspacePath（术语治理 0013 更名）。
	ProjectDir string `json:"project_dir"`
	// ProjectID 会话归属项目（空串 = 纯对话，不隶属任何项目）。
	ProjectID string `json:"project_id,omitempty"`
	// WorkspaceID 会话归属工作空间（执行面，对应一台设备；或服务端内置空间 ws-server）。
	// 空 = 纯对话，无执行面；与 ProjectDir（**项目目录**）互相独立：
	// WorkspaceID 决定「能力天花板」（能否执行命令/写文件），ProjectDir 只决定落盘目录。
	// 规划：工作空间将改为必填（纯对话亦须绑定，缺省绑服务端内置空间）。
	WorkspaceID string `json:"workspace_id,omitempty"`
	AccessMode  string `json:"access_mode"`
	ModelRef    string `json:"model_ref"`
	UserID      string `json:"user_id"`
	// Source 会话来源：ui（默认）/ openai（OpenAI 兼容端点的隐藏会话，P1）。
	Source        string    `json:"source,omitempty"`
	MessageCount  int64     `json:"message_count"`
	LastMessageAt *int64    `json:"last_message_at,omitempty"`
	TotalTokens   int64     `json:"total_tokens"`
	LastRunStatus RunStatus `json:"last_run_status,omitempty"`
	CreatedAt     int64     `json:"created_at"`
	UpdatedAt     int64     `json:"updated_at"`
	DeletedAt     *int64    `json:"deleted_at,omitempty"`
	// ArchivedAt 归档时间（NULL/缺省 = 未归档；对齐桌面端「归档中心」语义）。
	ArchivedAt *int64 `json:"archived_at,omitempty"`
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
	// Hidden 消息已随某次消息级回滚置灰（保留供审计；上下文组装排除；阶段 G）。
	Hidden    bool  `json:"hidden,omitempty"`
	CreatedAt int64 `json:"created_at"`
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

// Provider 模型厂商配置（主库）。APIKey 只作入参/出参（出参脱敏），落库仅存 api_key_enc 密文。
type Provider struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	BaseURL   string   `json:"base_url"`
	APIKey    string   `json:"api_key,omitempty"`
	Models    []string `json:"models"`
	Enabled   bool     `json:"enabled"`
	CreatedAt int64    `json:"created_at"`
	UpdatedAt int64    `json:"updated_at"`
	APIKeyEnc []byte   `json:"-"`
	HasAPIKey bool     `json:"has_api_key"`
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

// ToolEventExec 工具事件的执行面元信息（落库 executor / device_id / source_trust）。
//
// 用于「终态回写」时补充执行面归属：执行面的最终归属由实际执行者决定
// （端侧 → device/untrusted；静默降级 → server/trusted），因此必须在 Finish 阶段回写，
// 不能只在 Insert（pending）阶段写死 server。
type ToolEventExec struct {
	Executor    Executor
	DeviceID    string
	SourceTrust TrustLevel
}

// Normalized 补默认值：空 executor → server，空 trust → trusted。
//
// 端侧结果**必须**显式置 TrustUntrusted（不变量 4），不能依赖此默认值。
func (e ToolEventExec) Normalized() ToolEventExec {
	out := e
	if out.Executor == "" {
		out.Executor = ExecutorServer
	}
	if out.SourceTrust == "" {
		out.SourceTrust = TrustTrusted
	}
	return out
}
