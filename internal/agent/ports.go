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
	FinishToolEvent(ctx context.Context, id string, status model.ToolEventStatus, resultJSON, resultBlob, errMsg string, exec ToolEventExec) error

	ApprovalRules(ctx context.Context) ([]approval.Rule, error)
	AddApprovalRule(ctx context.Context, toolName, pattern string) error

	// AddRunUsage 回写单次 Run 的 token 用量（P1：runs 表预埋列的落点）。
	//
	// cachedTokens / isEstimated 是 0001_init.sql:44-45 预留列，此前从不写入，
	// 导致「缓存命中率」无分子分母、「估算 token」无法与真实值区分。
	// 现随调用一并落库（不隐藏估算值）。
	AddRunUsage(ctx context.Context, runID string, promptTokens, completionTokens, cachedTokens int64, isEstimated bool) error
}

// SummaryWriter 主库摘要回写（**与会话库不同事务，最终一致**）。
type SummaryWriter interface {
	TouchMessage(ctx context.Context, sessionID string, at int64) error
	AddTokensAndStatus(ctx context.Context, sessionID string, tokens int64, status model.RunStatus) error
}

// ============ 上下文压缩端口（可为 nil） ============

// CompactRequest 一次压缩请求。
type CompactRequest struct {
	SessionID string
	// ModelRef 会话绑定的模型引用。
	//
	// 为什么由引擎传入而不是实现自行反查：引擎在 req.ModelRef 里已经持有它，
	// 反查需要额外访问主库并可能落在错误的事务边界上。传入更简单也更准确。
	ModelRef string
	// Used 当前上下文已占用的估算 token。
	Used int
	// Limit 模型上下文窗口上限（<=0 表示未知，此时实现应跳过）。
	Limit int
	// Messages 会话全量消息（按 seq 升序，含 hidden 标记）。
	Messages []model.Message
	// Running 本 Run 是否仍在执行：true 时实现必须保护当前活跃轮次，
	// 不得把用户最新一次请求及其后续工具调用折叠进摘要。
	Running bool
}

// CompactResult 压缩结果。
type CompactResult struct {
	// Compacted 是否真的完成了一次压缩（写入了持久化摘要）。
	Compacted bool
	// StartSeq / EndSeq 本次折叠的区间（Compacted 时有效）。
	StartSeq int64
	EndSeq   int64
	// ReplacedMessages 被折叠的消息条数。
	ReplacedMessages int
	// SavedTokens 估算节省的 token（0 或负值表示无收益）。
	SavedTokens int
	// Summary 摘要正文（供事件与排障展示）。
	Summary string
}

// Compactor 上下文压缩端口。
//
// # 契约（实现方必须遵守）
//
//  1. **永不返回致命错误**：压缩是优化项。方法返回 error 时，引擎会记录并
//     继续用原历史执行，绝不会因压缩失败而让 Run 失败。
//  2. **不得修改入参**：Messages 是引擎正在使用的切片，实现必须只读。
//  3. **保护活跃轮次**：Running=true 时绝不能折叠最后一条 user 消息及其
//     后续工具调用——那等于让模型对当前任务失忆。
//  4. **幂等**：重复调用不得产生重叠的压缩区间；已压缩部分应跳过。
type Compactor interface {
	// NeedsCompaction 判断当前水位是否需要压缩（**必须无 IO**）。
	//
	// # 为什么单独暴露这个方法，而不是让引擎直接调 Compact
	//
	// 引擎每步都会问一次「现在要不要压缩」。若直接调 Compact，实现内部
	// 虽会先算水位再早退，但引擎侧为了构造 Messages 参数必须先
	// ListMessages（最多 1000 条）——那就是**每一步一次全表读**。
	// 因此把「判断」与「执行」拆开：判断是纯算术，执行才碰存储与模型。
	//
	// 契约：limit <= 0（窗口未知）时必须返回 false。
	NeedsCompaction(used, limit int) bool

	Compact(ctx context.Context, req CompactRequest) (CompactResult, error)
	// History 返回叠加压缩摘要后的历史（模型实际看到的内容）。
	//
	// 这是读取侧的唯一入口：只要经过它，"已压缩区间被摘要替代"这条语义
	// 就自动生效，调用点无需各自处理。
	History(ctx context.Context, sessionID string, msgs []model.Message) ([]llm.Message, error)
}

// ============ 审计日志端口（可为 nil） ============
//
// 这里用「窄结构 + 窄接口」而不是直接依赖 logvault 包：
//   - agent 包的设计铁律是「不 import store」，同理也不 import logvault；
//   - 上层用适配器把 LLMTrace → logvault.LLMRecord。
//
// 实现必须**非阻塞**（有界队列 + 队列满丢弃），因为这是 Run 关键路径上的调用，
// 对齐 docs/基础服务设计.md:301「快照写入不阻塞 TTFT」。

// LLMTrace 一次模型调用的追踪记录。
type LLMTrace struct {
	CreatedAt        int64
	SessionID        string
	RunID            string
	MessageID        string
	TraceID          string
	Model            string
	Provider         string
	Endpoint         string
	Payload          string // 请求报文 JSON（由适配器负责脱敏）
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	IsEstimated      bool
	TTFTMs           int64
	DurationMs       int64
	TokensPerSec     float64
	Status           string // ok | error | cancelled
	ErrorCode        string
	ErrorMessage     string
}

// ToolTrace 一次工具调用的追踪记录（会话库 tool_events 的查询副本）。
type ToolTrace struct {
	CreatedAt   int64
	SessionID   string
	RunID       string
	ToolEventID string
	ToolName    string
	Namespace   string
	ParamsJSON  string
	ResultJSON  string
	Status      string
	Risk        string
	Error       string
	DurationMs  int64
}

// TraceSink 审计日志写入端口。
type TraceSink interface {
	RecordLLM(t LLMTrace)
	RecordToolCall(t ToolTrace)
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
	// DeviceID 执行面归属设备（D7 执行面归属铁律）：非空时工具**必须**在该设备上执行。
	//
	// 空串 = 服务端本地执行面（ws-server 内置空间，或端侧未接入的部署）。
	// tools.DeviceRunner 会把它原样写入 DeviceTask.DeviceID，交给派发器严格路由：
	// 指定设备离线时**明确报错**，绝不回退到其他在线设备、也不降级本地执行。
	DeviceID string
}

// ToolRunResult 工具执行结果。
type ToolRunResult struct {
	Status     model.ToolEventStatus
	ResultJSON string
	ResultBlob string
	Error      string
	// Executor / DeviceID / SourceTrust 描述「这次结果由谁执行、是否可信」
	// （多端协同 HALO）：端侧结果一律 SourceTrust=untrusted，降级本地则 server/trusted。
	// 零值时引擎按 server + trusted 处理（与 C 阶段前行为完全一致）。
	Executor    model.Executor
	DeviceID    string
	SourceTrust model.TrustLevel
}

// ToolEventExec 工具事件的执行面元信息（落库 tool_events 的 executor/device_id/source_trust）。
//
// 之所以单独成类型：FinishToolEvent 增参后参数已达 7 个，聚合后调用点更清晰，
// 且后续新增（如复检结论）只需扩结构体、不再改端口签名。
// 定义在 model 包（跨层共享的数据类型），避免 agent/store/service 三方各建一份。
type ToolEventExec = model.ToolEventExec

// RuleSource 审批规则来源（可选端口）：引擎取规则的唯一入口。
//
// 之所以独立成端口而不是直接读 SessionDB.ApprovalRules：
//   - 规则有多个来源（会话级「本会话允许」+ 项目级声明规则），合并/去重策略必须只有一份实现；
//   - 引擎不应感知「项目」概念，也不该 import store 去查项目归属。
//
// nil 时引擎退回 SessionDB.ApprovalRules（旧行为完全不变）。
type RuleSource interface {
	RulesFor(ctx context.Context, sessionID string) ([]approval.Rule, error)
}

// Approver 审批闸门（approval.Gate 满足此接口）。
type Approver interface {
	Evaluate(req approval.Request) approval.Verdict
	Register(approvalID string) (<-chan approval.Decision, error)
	// RegisterWithDevice 带跨端归属登记（阶段 C-7）：requestedBy=发起端设备，
	// required=必须确认该审批的设备（空串 = 不绑定）。未接设备域时等价于 Register。
	RegisterWithDevice(approvalID, requestedBy, required string) (<-chan approval.Decision, error)
	Await(ctx context.Context, approvalID string) (approval.Decision, string)
	Resolve(approvalID string, d approval.Decision) bool
}

// LeaseManager 会话租约（阶段 C-6：跨端单写者）。
//
// 与本地 mutex 的关系：**两者并存、各管一层**。
//   - 本地 mutex：进程内同会话 Run 串行（保证消息 seq 与上下文一致）
//   - 租约：跨端可见 + 抢占语义（换持有者 epoch+1，旧端收 lease.revoked）
//
// holder 为空串表示**服务端本地执行面**持有（与 model.SessionLease 一致）。
// 端口为可选（nil 时引擎行为与 C 阶段之前完全一致）。
type LeaseManager interface {
	// Acquire 抢占或续期租约，返回生效后的租约。
	Acquire(ctx context.Context, sessionID, holder string) (model.SessionLease, error)
	// Renew 续期（同持有者且未过期则 epoch 不变）。
	Renew(ctx context.Context, sessionID, holder string) error
	// Release 释放（仅持有者本人生效，幂等）。
	Release(ctx context.Context, sessionID, holder string) error
}
