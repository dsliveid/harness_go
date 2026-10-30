package agent

import (
	"context"

	"harness-go/internal/approval"
	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/model"
	"harness-go/internal/prompt"
)

// 本文件定义引擎所需的**窄接口**（在使用方 agent 包定义，由 service 层适配 store 实现）。
//
// 依赖方向：agent 只依赖这些接口与 model/prompt/llm/events/approval，
// **不 import store**，因此可单测（用 fake 实现）且可替换存储。

// SessionProvider 按会话 ID 取得会话库句柄。
type SessionProvider interface {
	Session(ctx context.Context, sessionID string) (SessionDB, error)
}

// SessionDB 会话库操作（消息 / Run / 工具事件 / 审批规则）。
type SessionDB interface {
	AppendMessage(ctx context.Context, m *model.Message) error
	ListMessages(ctx context.Context, afterSeq int64, limit int) ([]model.Message, error)

	GetRun(ctx context.Context, id string) (*model.Run, error)
	TransitionRun(ctx context.Context, id string, to model.RunStatus, stopReason, errCode, errMsg string) error

	InsertToolEvent(ctx context.Context, ev *model.ToolEvent) error
	FinishToolEvent(ctx context.Context, id string, status model.ToolEventStatus, resultJSON, resultBlob, errMsg string) error

	ApprovalRules(ctx context.Context) ([]approval.Rule, error)
	AddApprovalRule(ctx context.Context, toolName, pattern string) error
}

// SummaryWriter 主库摘要回写（**与会话库不同事务，最终一致**）。
type SummaryWriter interface {
	TouchMessage(ctx context.Context, sessionID string, at int64) error
	AddTokensAndStatus(ctx context.Context, sessionID string, tokens int64, status model.RunStatus) error
}

// EventPublisher 事件发布（per-Run）。
type EventPublisher interface {
	Publish(runID string, typ events.Type, payload any) (events.Event, error)
	CloseRun(runID string)
}

// ModelResolver 解析 model_ref → provider + 具体模型名。
type ModelResolver interface {
	Resolve(ctx context.Context, modelRef string) (llm.Provider, string, error)
}

// ContextBuilder 上下文组装。
type ContextBuilder interface {
	Build(in prompt.Input) prompt.BuildResult
}

// ToolRunner 工具执行（A 阶段可为 nil：无工具场景）。
type ToolRunner interface {
	Run(ctx context.Context, req ToolRunRequest) (ToolRunResult, error)
}

// ToolCatalog 工具目录（可选端口）：提供工具风险级，避免引擎硬编码风险。
//
// 由 internal/tools 的 Runner 适配实现；未注入时引擎按 write 保守处理。
type ToolCatalog interface {
	Risk(toolName string) (model.Risk, bool)
}

// ToolRunRequest 工具执行请求。
type ToolRunRequest struct {
	RunID       string
	SessionID   string
	ToolEventID string
	ToolName    string
	ArgsJSON    string
	AccessMode  string // readonly | confirm | full（工具可据此自行收紧行为）
}

// ToolRunResult 工具执行结果。
type ToolRunResult struct {
	Status     model.ToolEventStatus
	ResultJSON string
	ResultBlob string
	Error      string
}

// Approver 审批闸门（approval.Gate 满足此接口）。
type Approver interface {
	Evaluate(req approval.Request) approval.Verdict
	Register(approvalID string) (<-chan approval.Decision, error)
	Await(ctx context.Context, approvalID string) (approval.Decision, string)
	Resolve(approvalID string, d approval.Decision) bool
}
