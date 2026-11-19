// Package agent 是 Run 执行引擎（内核核心）。
//
// 职责：驱动「组装上下文 → 调模型 → 流式转发 → 工具调用与审批 → 落库 → 状态迁移」。
//
// 铁律（docs/架构设计文档.md §3.3 / §5）：
//   - 状态迁移单点收口（本包 state.go 的矩阵）
//   - **delta 不落库**，只落里程碑（消息完成 / tool.started / result / 审批 / Run 状态）
//   - 消息最终内容以 DB 为准，事件流只是预览
//   - 慢客户端不阻塞 Run（events 包已保证）
//   - 不做断点续跑：进程重启把未完成 Run 置 interrupted（ADR-009）
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"harness-go/internal/approval"
	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/model"
	"harness-go/internal/prompt"
)

// Deps 引擎依赖（显式构造注入，无 DI 框架）。
type Deps struct {
	Sessions      SessionProvider
	Summary       SummaryWriter // 可为 nil（不写摘要）
	Events        EventPublisher
	Models        ModelResolver
	Builder       ContextBuilder
	Tools         ToolRunner  // 可为 nil（无工具场景）
	Catalog       ToolCatalog // 可为 nil（nil 时工具风险按 write 保守处理）
	Gate          Approver
	Leases        LeaseManager // 可为 nil（nil 时不做跨端租约，行为与 C 阶段前一致）
	MaxConcurrent int
	MaxSteps      int
	// StepNoticeEvery 步数进度提示间隔：每 N 步向模型注入一行
	// 「当前步数 X/总步数 N」，让模型感知剩余预算、在接近上限时主动收敛。
	// <=0 时回落默认 10；显式关闭请配 0 以外的语义（引擎内 <=0 即视为默认值）。
	StepNoticeEvery int
	ToolTimeout     time.Duration
	Now             func() int64
	// AccessModeProvider 实时读取会话访问模式（可选）。
	// 对齐桌面端语义：顶栏「变更前确认/完全访问」切换即时生效——对话进行中同样立即生效，
	// 因此每次审批决策前重读一次；读取失败回落 RunRequest.AccessMode（Run 启动快照）。
	// nil 时维持旧行为（始终用启动快照）。
	AccessModeProvider func(ctx context.Context, sessionID string) (approval.AccessMode, error)
	// Rules 审批规则来源（可选）：非 nil 时由它统一提供「会话级 + 项目级」合并后的规则集；
	// nil 时退回 sdb.ApprovalRules（仅会话级，旧行为不变）。
	Rules RuleSource
	// RiskOverride 按参数升级工具风险级（可选）：入参 (工具名, 参数 JSON, 契约静态风险级)，
	// 返回实际用于审批的风险级。用于高危命令特征检测（run_command 命中 → danger）；
	// nil 时始终用契约静态风险级。
	RiskOverride func(toolName string, argsJSON []byte, base model.Risk) model.Risk
	// Traces 审计日志写入（可选，nil 时不记任何审计日志）。
	//
	// 实现必须非阻塞：这是 Run 关键路径上的调用，写入侧有界队列 + 满即丢弃。
	// 对齐 docs/基础服务设计.md:301「快照写入不阻塞 TTFT」。
	Traces TraceSink
	// TraceID 取当前请求的 trace_id（可选）：nil 或返回空串时审计日志的
	// trace_id 字段为空（不影响功能，只是无法从日志反查请求链路）。
	TraceID func(ctx context.Context) string
	// Compactor 上下文压缩（可选，nil = 不压缩，行为与改造前一致）。
	//
	// # 为什么在引擎层而不是 BudgetGate 层
	//
	// BudgetGate 只有「决策权」：它能判断"该压缩了"，但没有存储与模型权限
	// （service 层的 BudgetGate 刻意不碰 store 与 llm，那是它保持可测的前提）。
	// 而压缩必须写持久化摘要表并调模型 —— 这在编排层（引擎）才成立。
	//
	// 因此分工是：
	//   BudgetGate 第⑤级 → 产出 NeedCompress 信号（已有）
	//   引擎消费该信号     → 调 Compactor 真正执行（本节新增）
	//   Compactor 完成后   → 引擎重建 history 并继续（对模型透明）
	Compactor Compactor
	// MaxContextTokens 模型上下文窗口上限（压缩水位基准）。
	//
	// <=0 表示未知：此时压缩会被跳过（实现层约定），行为退回改造前。
	// 之所以仍保留"可关闭"，是因为窗口确实可能未知；但**默认值必须是真实值**
	// （见 config 的 compaction.max_context_tokens），否则又会变成
	// 「机制存在但常年休眠」——那正是本次事故的制度性根因。
	MaxContextTokens int
}

// maxCompactionsPerRun 单个 Run 内允许的压缩次数上限。
//
// 为什么必须设上限：压缩触发后若历史仍偏长，下一步可能再次触发。
// 每触发一次都是一次完整的摘要模型调用（输入是被折叠区间全文）。
// 长任务若每步都压缩一次，会变成"花在摘要上的钱比正文还多"。
// 3 次足以应对「第一步就满窗 → 折叠 → 又满 → 再折叠」的极端情况。
const maxCompactionsPerRun = 3

// Engine Run 执行引擎。
type Engine struct {
	deps  Deps
	sem   chan struct{} // 全局并发闸门（超出则排队，不拒绝）
	locks sync.Map      // sessionID → *sync.Mutex（per-session 串行）
}

// New 创建引擎。
func New(deps Deps) (*Engine, error) {
	if deps.Sessions == nil {
		return nil, errors.New("agent: 缺少 Sessions 依赖")
	}
	if deps.Events == nil {
		return nil, errors.New("agent: 缺少 Events 依赖")
	}
	if deps.Models == nil {
		return nil, errors.New("agent: 缺少 Models 依赖")
	}
	if deps.Builder == nil {
		return nil, errors.New("agent: 缺少 Builder 依赖")
	}
	if deps.Gate == nil {
		return nil, errors.New("agent: 缺少 Gate 依赖")
	}
	if deps.MaxConcurrent <= 0 {
		deps.MaxConcurrent = 8
	}
	if deps.MaxSteps <= 0 {
		deps.MaxSteps = 100 // 默认 100 步（原 20：复杂任务易被误截断）
	}
	if deps.StepNoticeEvery <= 0 {
		deps.StepNoticeEvery = 10 // 每 10 步注入一次步数进度提示
	}
	if deps.ToolTimeout <= 0 {
		deps.ToolTimeout = 120 * time.Second
	}
	if deps.Now == nil {
		deps.Now = func() int64 { return time.Now().UnixMilli() }
	}
	return &Engine{deps: deps, sem: make(chan struct{}, deps.MaxConcurrent)}, nil
}

// RunRequest 一次 Run 的执行参数（由 service 层组装）。
type RunRequest struct {
	RunID          string
	SessionID      string
	UserInput      string
	ModelRef       string
	AccessMode     approval.AccessMode
	SystemBase     string
	SOP            []string
	ProjectSummary string
	Tools          []prompt.ToolRef
	// ActiveTools 本轮经动态检索命中、需注入全量定义的 deferred 工具名（阶段 D-2）。
	// 铁律：只影响「注入深度」，不影响「能否执行」（宽进严出）——
	// 未列出的 deferred 工具仍可被调用，只是模型看不到其 schema。
	ActiveTools []string
	// MemoryCore 记忆常驻块（D-3：L0 核心约束 + L1 项目摘要），进 system 稳定前缀。
	// 由 service 层每 Run 召回一次后传入；引擎只透传不解释。
	MemoryCore string
	// MemoryTail 记忆动态块（D-3：L2/L3 检索命中），置尾注入；预算降级第①级的省略对象。
	MemoryTail string
	// LockKey 会话锁键（阶段 F 嵌套 Run）：空 = 按 SessionID 加锁（默认）；
	// 非空 = 用它做锁键。子 Run 传入「sessionID|spawn|<runID>」绕开父 Run 持有的
	// 会话锁（同一 goroutine 链上父等待子，若同键必死锁）；父阻塞等待期间
	// 双方不并发追加消息（seq 安全性不变），子与子之间天然互斥（键唯一）。
	LockKey string
	// LeaseHolder 会话租约持有者（阶段 C-6）：设备 ID；空串 = 服务端本地执行面。
	// 端侧发起 Run 时由 service 层填入该设备 ID，抢占旧端 epoch+1 并推送 lease.revoked。
	LeaseHolder string
	// RequesterDevice 发起本次 Run 的执行端设备（阶段 C-7 审批跨端仲裁的审计来源）。
	// 未填时回退 LeaseHolder；两者都空表示服务端本地执行面（审批不绑定设备）。
	RequesterDevice string
	// GuidanceCh 运行中引导通道（需求 §4.2，对齐桌面端「引导」）：service 层把已落库的
	// 引导用户消息推入该通道，引擎在**轮次边界**消费——当前流结束后模型即看到该引导。
	// 消息已由 service 层落库，引擎只回灌上下文不重复落库；nil 表示本 Run 不支持引导。
	GuidanceCh <-chan *model.Message
	// ToolPolicy 工具能力策略（工作空间能力天花板，协议 §4.1）：三态，由 service 层唯一裁决。
	//
	// 被拒工具在 Gate.Evaluate **之前**立即拒绝并落库，不进入 waiting_approval、
	// 不阻塞 Gate.Await，Run 继续执行（拒绝粒度是工具调用级）。
	//
	// 铁律：判定唯一入口在 service 层（按 session.workspace_id 所指 Workspace 的运行时状态），
	// 引擎只消费结果，不做第二套判定（避免两处实现漂移）。
	ToolPolicy ToolPolicy
	// PolicyReason 策略拒绝文案（展示给模型，避免它反复重试）。空串时由引擎取默认文案。
	PolicyReason string
	// ExecutionDeviceID 执行面归属设备（D7 执行面归属铁律）：工具**必须**在这台设备上执行。
	//
	// 空串 = 服务端内置空间（ws-server），允许本地执行。
	// 非空 = 会话绑定的工作空间所归属的设备，引擎逐层透传至 DeviceTask.DeviceID。
	//
	// 🚨 为什么必须显式传递：派发器在未指定设备时会退化为「全部在线设备按 ID 排序取首个」，
	// 导致会话绑手机 A、工具却在桌面端 B 上执行（错误执行，非功能缺失）。
	ExecutionDeviceID string
}

// ToolPolicy 工具能力策略（工作空间能力天花板的三态表达）。
//
// 为什么必须是三态而不是布尔：能力边界客观上有三种，用布尔会逼迫其中一档被错误合并。
// 历史教训：旧的 `DenyTools bool` 只拦 command + danger 两级，`write` 级（write_file /
// edit_file）被静默放行——移动端「禁止文件写入」的契约形同虚设。
//
// 三态的语义边界（互不重叠）：
//   - full     ：允许全部工具（在线设备空间 / 服务端内置空间，仍受 kind 与审批链约束）；
//   - readonly ：仅允许 readonly 级（移动端：仅对话 + 图片识别，禁写禁命令）；
//   - none     ：拒绝**全部**工具（未绑定设备 / 设备离线 / 设备已删除或已解绑）。
//
// 🚨 none 不是「少给点能力」，而是安全边界：这些空间没有可用的执行宿主，
// 任何工具调用都只能落到服务端本地执行，等于把用户的设备操作偷偷跑在服务端上（见 D8）。
type ToolPolicy string

const (
	// ToolPolicyFull 允许全部工具（受 kind 与审批链约束）。
	ToolPolicyFull ToolPolicy = "full"
	// ToolPolicyReadonly 仅允许 readonly 级工具（移动端）。
	ToolPolicyReadonly ToolPolicy = "readonly"
	// ToolPolicyNone 拒绝全部工具（无可用执行宿主）。
	ToolPolicyNone ToolPolicy = "none"
)

// Allows 判断该策略下 risk 级工具是否可执行。
//
// 零值（空串）按 full 处理：保证未接入工作空间判定的既有调用点（含测试）零回归。
func (p ToolPolicy) Allows(risk model.Risk) bool {
	switch p {
	case ToolPolicyNone:
		return false
	case ToolPolicyReadonly:
		return risk == model.RiskReadonly
	default:
		return true
	}
}

// defaultPolicyReason 策略拒绝时的兜底文案（模型可见，必须说清「为什么」与「怎么办」）。
func defaultPolicyReason(p ToolPolicy) string {
	if p == ToolPolicyReadonly {
		return "当前工作空间仅支持只读操作与对话，禁止执行命令或写入文件"
	}
	return "当前工作空间未连接可用的执行设备，无法调用工具"
}

// detachedCtx 返回一个**不受父 ctx 取消影响**的记账用 ctx（5s 超时兜底不泄漏）。
//
// 为什么必须有（2026-11 实测缺陷）：Run 取消（用户停止 / 设备离线 / 请求断开）后，
// 父 ctx 立刻 Done。若收尾写入（终态迁移、摘要回写、工具事件收口、消息落库）
// 复用它，database/sql 会直接返回 context canceled 而**不执行 SQL**，且调用点普遍
// 以 `_ =` 忽略错误 —— 结果就是「Run 明明停了，库里还挂着 running / pending」，
// 前端刷新后据此重建为「正在对话中」、工具卡片永远「执行中…」。
//
// 铁律：取消只该停止「继续干活」，绝不该吞掉「记账」。
func detachedCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

// dbctx 单次记账用的脱离取消 ctx（不关心 cancel：5s 超时定时器自行回收）。
//
// 与 detachedCtx 同一目的，只是为了在 `sdb.Xxx(ctx, ...)` 调用点做**最小改动**替换：
// 每次写入各取一个新 ctx，绝不复用一个长寿命 ctx（工具可能跑几分钟，
// 共享的 5s 超时会在收尾时早已到期 → 又变成「写不进去」）。
func dbctx(ctx context.Context) context.Context {
	c, _ := detachedCtx(ctx)
	return c
}

// Execute 执行 Run，阻塞直到终态。
//
// 返回 error 仅表示「执行过程异常」，Run 的最终状态始终以数据库为准。
func (e *Engine) Execute(ctx context.Context, req RunRequest) error {
	if req.RunID == "" || req.SessionID == "" {
		return errors.New("agent: RunID 与 SessionID 不能为空")
	}

	// ① per-session 串行：同一会话的 Run 排队执行（保证消息 seq 与上下文一致）。
	// 嵌套 Run（F）：LockKey 非空时以它为锁键（子 Run 绕开父持有的会话锁，
	// 父在 Wait 中阻塞、不并发追加，seq 安全性由「父等待子」的串行性保证）。
	lockKey := req.LockKey
	if strings.TrimSpace(lockKey) == "" {
		lockKey = req.SessionID
	}
	unlock := e.lockSession(lockKey)
	defer unlock()

	// ② 全局并发闸门：满则排队等待（超并发不拒绝，符合架构 §3.2）
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		// 排队期间就被取消：Run 已建（pending），必须如实收口，否则 active-run 永远命中它。
		e.finishCancelled(req, "queue_canceled")
		return ctx.Err()
	}

	// ③ 跨端会话租约（C-6）：与本地 mutex 并存，解决「跨端可见 + 抢占 epoch+1」。
	// 抢占失败不是错误（抢占本身即语义，旧持有者经 lease.revoked 得知）；
	// 租约读失败也不阻断 Run（协调信息，单机正确性由本地 mutex 保证）。
	if e.deps.Leases != nil {
		holder := leaseHolder(req)
		if _, lErr := e.deps.Leases.Acquire(ctx, req.SessionID, holder); lErr != nil {
			e.publishLeaseWarn(req.RunID, "acquire_failed", lErr)
		}
		defer func() {
			// 释放用独立 ctx：Run 可能因取消而 ctx.Done，释放仍必须尽力完成
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = e.deps.Leases.Release(rctx, req.SessionID, holder)
		}()
	}

	// 记账类操作一律脱离取消 ctx（见 detachedCtx 注释）：即便此刻 Run 已被取消，
	// 仍必须能读到 Run 行并写终态，否则前端刷新后会把它当成「进行中」。
	dctx := dbctx(ctx)
	sdb, err := e.deps.Sessions.Session(dctx, req.SessionID)
	if err != nil {
		return fmt.Errorf("agent: 打开会话库失败: %w", err)
	}

	run, err := sdb.GetRun(dctx, req.RunID)
	if err != nil {
		return fmt.Errorf("agent: 读取 Run 失败: %w", err)
	}
	// 已被其它路径（StopRun 同步收口 / 启动恢复）收口为终态：不再抢占 running，
	// 也不改动已有终态（幂等交由 store.Transition 兜底），但如实返回错误供上层观测。
	if run.Status.IsTerminal() {
		return fmt.Errorf("agent: 非法状态迁移 %s → %s（Run 已结束）", run.Status, model.RunRunning)
	}
	// 状态机校验（单点收口）
	if err := CheckTransition(run.Status, model.RunRunning); err != nil {
		return err
	}
	if err := e.transition(dctx, sdb, req.RunID, model.RunRunning, "", "", ""); err != nil {
		return err
	}
	e.publishStatus(req.RunID, req.SessionID, model.RunRunning, "", "", "", nil)

	usage := &runUsage{}
	final, stopReason, errCode, errMsg, tokens := e.loop(ctx, sdb, req, usage)

	// 终态收口：**绝不能因 ctx 已取消而失败早退**。
	//
	// 历史缺陷（勿回退）：这里原用 req ctx 调 Transition，取消后 SQL 不执行 → 报错 →
	// 直接 return，跳过下面两步 Summary 回写与终态事件 → 库里永远 running，
	// 前端刷新（F5）后根据 /active-run 重建为「正在对话中」。三条路径统一走 detachedCtx。
	tctx, tcancel := detachedCtx(ctx)
	defer tcancel()
	var transitionErr error
	if err := e.transition(tctx, sdb, req.RunID, final, stopReason, errCode, errMsg); err != nil {
		transitionErr = err
	}
	// ③ 摘要回写主库（**不同事务、最终一致**；失败只影响列表新鲜度）
	//
	// ⚠️ 顺序约束（可见性竞态，勿随意调整）：必须在「终态事件发布 + CloseRun」之前完成。
	// 客户端一旦从终态事件/连接关闭得知 Run 已结束，就会立刻回查会话列表；
	// 若回写滞后于终态可观测，则会出现「Run 已 completed，但 last_run_status 仍为空」
	// 的瞬时不一致（TestEndToEndChatFlow 曾因此以约 60% 概率偶发失败）。
	if e.deps.Summary != nil {
		_ = e.deps.Summary.AddTokensAndStatus(tctx, req.SessionID, tokens, final)
	}

	e.publishStatus(req.RunID, req.SessionID, final, stopReason, errCode, errMsg, usage)
	e.deps.Events.CloseRun(req.RunID)

	if transitionErr != nil {
		return fmt.Errorf("agent: 终态迁移失败: %w (原始状态=%s)", transitionErr, final)
	}
	if final == model.RunFailed {
		return fmt.Errorf("agent: Run 失败: %s %s", errCode, errMsg)
	}
	return nil
}

// assertMutable 判断该 Run 是否仍可写入新消息（终态不可变守卫）。
//
// # 为什么需要这个守卫，以及为什么必须只有一处
//
// Run 的终态（cancelled / failed / completed / interrupted）一旦写入，
// 就不该再有任何业务写入。但真实路径里，终态可能由**别处**触发：
//   - 用户点停止 → service 层直接迁移 Run 状态；
//   - 设备离线 / 请求断开 → 上层取消；
//   - 会话被其他端接管。
//
// 而引擎此时可能正卡在一次耗时几十秒的工具执行里。工具返回后若不加校验
// 就把结果落库，就会出现「Run 已 cancelled，messages 却仍在增长」。
//
// 真实事故（会话 9ee34e00）：runs.ended_at=1795058266409 已 cancelled，
// 但 messages seq107/108 的 created_at 是 …320130/…320132，迟 53.7 秒。
//
// # 为什么收敛在这一个函数
//
// 若在每个 AppendMessage 前散落 `if run.Status.IsTerminal() { return }`，
// 迟早会有新的写入点忘记加，缺陷就会以"新的一种表现"复发。收敛到单一
// 断言点后，新增写入路径只要调用它，就不会绕过不变量。
//
// # 为什么用 detached ctx 读取
//
// 校验发生在取消路径上，此时父 ctx 已 Done。复用父 ctx 查库会直接返回
// context canceled 而不执行 SQL，导致守卫在**最需要它的时刻失效**。
func (e *Engine) assertMutable(ctx context.Context, sdb SessionDB, runID string) bool {
	rctx, cancel := detachedCtx(ctx)
	defer cancel()
	run, err := sdb.GetRun(rctx, runID)
	if err != nil {
		// 读不到状态时保守放行？不。宁可少写一条消息，也不能污染已终态的历史。
		// 但要注意：若库本身不可用，上层 AppendMessage 也会失败并如实报错，
		// 因此这里返回 false 不会掩盖真实故障。
		return false
	}
	return !run.Status.IsTerminal()
}

// finishCancelled 前置失败路径的 Run 收口（并发闸门排队被取消 / 打开会话库失败前）。
//
// 必须存在：这些路径原本直接 return，Run 停在 pending 永不收口 ——
// /active-run 会一直把它当作「进行中」返回，前端刷新后重建为正在对话。
func (e *Engine) finishCancelled(req RunRequest, stopReason string) {
	ctx, cancel := detachedCtx(context.Background())
	defer cancel()
	sdb, err := e.deps.Sessions.Session(ctx, req.SessionID)
	if err != nil {
		return
	}
	if run, err := sdb.GetRun(ctx, req.RunID); err == nil && run.Status.IsTerminal() {
		return
	}
	_ = e.transition(ctx, sdb, req.RunID, model.RunCancelled, stopReason, "", "")
	if e.deps.Summary != nil {
		_ = e.deps.Summary.AddTokensAndStatus(ctx, req.SessionID, 0, model.RunCancelled)
	}
	e.publishStatus(req.RunID, req.SessionID, model.RunCancelled, stopReason, "", "", nil)
	e.deps.Events.CloseRun(req.RunID)
}

// runUsage per-Run 用量累计（P1：落 runs 表 + 终态 run.status 事件出栈）。
type runUsage struct {
	prompt     int64
	completion int64
	// cached 缓存命中 token（此前从不写入 runs.cached_tokens，导致命中率无分母）。
	cached int64
	// estimated 本次 Run 是否含估算用量（宁可标记估算，也不伪装成真实值）。
	estimated bool
}

// loop 主循环：组装 → 流式 → 落消息 → 工具 → 回灌，直到终态或步数上限。
// usage 非空时累计本 Run 的输入/输出 token（供落库与终态事件），nil 时只丢弃。
func (e *Engine) loop(ctx context.Context, sdb SessionDB, req RunRequest, usage *runUsage) (
	final model.RunStatus, stopReason, errCode, errMsg string, tokens int64,
) {
	// P1：per-Run 用量累计，返回前统一回写 runs 表（取消路径也记账——
	// WithoutCancel 保证写入不被请求取消吞掉）。
	defer func() {
		if usage != nil {
			_ = sdb.AddRunUsage(context.WithoutCancel(ctx), req.RunID, usage.prompt, usage.completion, usage.cached, usage.estimated)
		}
	}()

	history, err := e.loadHistory(ctx, sdb)
	if err != nil {
		return model.RunFailed, "", "history_load_failed", err.Error(), 0
	}

	userInput := req.UserInput
	// 本 Run 的压缩次数计数器（**局部变量而非 Engine 字段**）。
	//
	// 为什么不能放 Engine 上：Engine 是全进程单例且被多个会话并发复用
	// （MaxConcurrent 8），共享计数器会同时造成两个问题：
	//   ① 数据竞争（-race 必报）；
	//   ② 语义错误——A 会话压缩 3 次后，B 会话的压缩也被一起掐掉。
	// 放在 loop 局部，天然按 Run 隔离且随 Run 结束自动归零。
	compactionsThisRun := 0
	for step := 0; step < e.deps.MaxSteps; step++ {
		if ctx.Err() != nil {
			return model.RunCancelled, "context_canceled", "", "", tokens
		}
		// 终态不可变（单一断言点之一）：每轮循环起始校验一次。
		// 防止 Run 已在别处（取消/超时/接管）进入终态后，本循环继续产出消息，
		// 造成"Run 已 cancelled，messages 却还在增长"的历史污染。
		if !e.assertMutable(ctx, sdb, req.RunID) {
			return model.RunCancelled, "run_already_terminal", "", "", tokens
		}
		// 每步续期会话租约（C-6）：长 Run 不因租约 TTL 到期而被误接管。
		e.renewLease(ctx, req)

		// 轮次边界消费运行中引导（§4.2）：当前流结束后模型即看到引导内容
		e.drainGuidance(ctx, sdb, req, &history)

		// 步数进度提示（每 StepNoticeEvery 步注入一次）：
		// 让模型感知剩余预算，在接近上限时主动收敛输出。
		// 走 DynamicTail 通道（Builder 保证置尾到本轮 user 消息前），
		// **绝不进 system 稳定前缀**，否则每步变化的进度文本会击穿前缀缓存。
		dynamicTail := stepNotice(step, e.deps.MaxSteps, e.deps.StepNoticeEvery)

		res := e.deps.Builder.Build(prompt.Input{
			Model:          req.ModelRef,
			SystemBase:     req.SystemBase,
			SOP:            req.SOP,
			ProjectSummary: req.ProjectSummary,
			History:        history,
			Tools:          req.Tools,
			ActiveTools:    req.ActiveTools,
			MemoryCore:     req.MemoryCore,
			MemoryTail:     req.MemoryTail,
			DynamicTail:    dynamicTail,
			UserInput:      userInput,
		})

		// D-4 预算闸门：降级后仍超预算 → 明确失败该 Run（严禁静默截断）。
		// 降级动作本身已记录在 res.Degrade，这里发非致命事件供前端/日志观察。
		if len(res.Degrade) > 0 {
			e.publish(req.RunID, events.TypeError, map[string]any{
				"scope":   "budget_gate",
				"code":    "agent.budget_degrade",
				"message": strings.Join(degradeDetails(res.Degrade), "；"),
				"level":   int(res.Degrade[len(res.Degrade)-1].Level),
			})
		}
		// ⑤ 上下文压缩：**独立的容量治理**，不再挂在预算法失败之下。
		//
		// # 这里修的是一个真实缺陷（2026-11 自查发现）
		//
		// 上一版把压缩挂在 `res.OverBudget` 条件下，而：
		//     prompt/builder.go: overBudget := MaxTotal > 0 && Total > MaxTotal
		//     config 默认值:     budget.max_total: 0
		// 于是 OverBudget 恒为 false → 压缩**在默认配置下永不执行**。
		// 讽刺的是，被修的原始缺陷正是"机制存在但常年休眠"（max_total=0 让
		// 五级降级从不触发，第五级又无消费者）——我在同一处换了个形态重犯。
		//
		// # 根因是轴混用
		//
		//   - 轴 A「窗口容量」：模型物理上能装多少（MaxContextTokens）。
		//     装不下就是装不下，与"愿不愿意花钱"无关。
		//   - 轴 B「预算成本」：愿意为本次 Run 花多少（max_total）。是主动约束。
		//
		// 压缩解决的是轴 A 的问题，却继承了轴 B 的开关。现在按轴 A 独立判断：
		// 只看 used 是否达到窗口的 80%，与 max_total 彻底解耦。
		//
		// # 为什么顺带限制单 Run 压缩次数
		//
		// 压缩触发后若历史仍偏长，下一步可能再次触发。每触发一次就是一次
		// 额外模型调用，长任务会变成"每步一次摘要"。因此按 Run 设上限。
		if e.deps.Compactor != nil && compactionsThisRun < maxCompactionsPerRun &&
			e.deps.Compactor.NeedsCompaction(res.Breakdown.Total, e.deps.MaxContextTokens) {
			compactionsThisRun++
			if newHistory, used := e.tryCompact(ctx, sdb, req, res, history); used {
				// 压缩成功：用重建后的历史重新组装一次。
				// 只重建一次——若压缩后仍超窗，那就是真的装不下了，
				// 交给上游 provider 报错（此时已无更强的本地手段）。
				history = newHistory
				res = e.deps.Builder.Build(prompt.Input{
					Model:          req.ModelRef,
					SystemBase:     req.SystemBase,
					SOP:            req.SOP,
					ProjectSummary: req.ProjectSummary,
					History:        history,
					Tools:          req.Tools,
					ActiveTools:    req.ActiveTools,
					MemoryCore:     req.MemoryCore,
					MemoryTail:     req.MemoryTail,
					DynamicTail:    dynamicTail,
					UserInput:      userInput,
				})
			}
		}
		// 预算法失败必须留在压缩**之后**：压缩能救回来的场景不该直接判死。
		if res.OverBudget {
			return model.RunFailed, "", "budget_exhausted",
				"上下文超出总预算，五级降级用尽后仍超限（严禁静默截断）", tokens
		}

		provider, modelName, err := e.deps.Models.Resolve(ctx, req.ModelRef)
		if err != nil {
			return model.RunFailed, "", "model_resolve_failed", err.Error(), tokens
		}
		res.Request.Model = modelName

		out, status, code, failMsg, used := e.streamOnce(ctx, req, provider, res)
		tokens += used
		if usage != nil {
			usage.prompt += out.promptTokens
			usage.completion += out.completionTokens
			usage.cached += out.cachedTokens
			if out.estimated {
				usage.estimated = true
			}
		}
		if status != "" { // 非空表示本轮异常终止
			return status, "", code, failMsg, tokens
		}

		// 落 assistant 消息（里程碑事件，delta 不落库）
		assistantMsg := &model.Message{
			ID:        newID(),
			RunID:     req.RunID,
			Role:      model.RoleAssistant,
			Content:   out.content,
			Reasoning: out.reasoning,
			CreatedAt: e.deps.Now(),
		}
		// 消息级用量落库（`messages.token_count` / `messages.usage_json`）。
		//
		// 🚨 历史悠久缺陷（2026-11 修复）：这两列从建表起就**从未被写入过**，
		// 导致「历史回放」路径上 token 用量与耗时全为空 —— 前端 TurnFooterBar
		// 因 totalTokens===0 直接 return null，用户看到的是「旧对话底部指标栏
		// 整条消失」，只有刚刚流式结束的那一轮有数字，刷新即丢。
		// TokenCount 口径 = 输入 + 输出（整条消息总量），与前端 `tokens` 语义一致。
		assistantMsg.TokenCount = int(out.promptTokens + out.completionTokens)
		assistantMsg.UsageJSON = messageUsageJSON(out)
		if len(out.toolCalls) > 0 {
			// 绝不能「marshal 失败就什么都不写」：那会让 assistant 消息丢掉 tool_calls 声明，
			// 而后续 tool 结果照常落库 → 历史里出现孤儿 tool 消息 → 上游每次重放都 400。
			// 见 sanitizeToolCalls 的说明（真实事故：会话被永久污染）。
			assistantMsg.ToolCallsJSON = marshalToolCalls(e, req, out.toolCalls)
		}
		if err := sdb.AppendMessage(dbctx(ctx), assistantMsg); err != nil {
			return model.RunFailed, "", "persist_failed", err.Error(), tokens
		}
		if e.deps.Summary != nil {
			_ = e.deps.Summary.TouchMessage(ctx, req.SessionID, e.deps.Now())
		}
		e.publish(req.RunID, events.TypeMessageEnd, map[string]any{
			"message_id": assistantMsg.ID,
			"content":    out.content,
			"seq":        assistantMsg.Seq,
			"reasoning":  out.reasoning,
			// 步级用量（协议向后兼容：仅新增字段，不改既有形状）。
			// 前端 TurnFooterBar 的「本轮 token / 缓存命中率 / 步耗时」全靠这几项：
			// 缺它们时前端只能取 0 → 指标栏整条不渲染（用户反馈的「没有按原型显示」）。
			"token_count":       assistantMsg.TokenCount,
			"prompt_tokens":     out.promptTokens,
			"cached_tokens":     out.cachedTokens,
			"completion_tokens": out.completionTokens,
			"duration_ms":       out.durationMs,
		})

		if len(out.toolCalls) == 0 {
			return model.RunCompleted, "", "", "", tokens
		}

		// 工具调用：回灌 assistant(tool_calls) + 逐条 tool 结果
		history = append(history, toLLMMessage(assistantMsg))
		for _, tc := range out.toolCalls {
			toolMsg, cancelled := e.execTool(ctx, sdb, req, tc)
			toolMsg.RunID = req.RunID
			toolMsg.CreatedAt = e.deps.Now()

			// 终态不可变（单一断言点之二，**最关键的一处**）：
			// 工具执行可能耗时数十秒。期间用户点停止，Run 会被别处迁移到
			// cancelled 终态；而工具结果此刻才返回。若此处不校验就落库，
			// 就会出现事故现场：runs.ended_at=…266409 已 cancelled，
			// messages 却在 …320130 又长出两条。
			//
			// 处理策略（三者必须同时成立）：
			//   - tool_events 仍由 execTool 内部写完（审计不丢，前端卡片能收口）；
			//   - 本轮结果**不**写 messages（不污染模型上下文与后续重放）；
			//   - 立即以 cancelled 收口，不再进入下一轮。
			if !e.assertMutable(ctx, sdb, req.RunID) {
				return model.RunCancelled, "run_terminal_during_tool", "", "", tokens
			}
			// tool 消息同样落库（消息最终内容以 DB 为准）
			if err := sdb.AppendMessage(dbctx(ctx), toolMsg); err != nil {
				return model.RunFailed, "", "persist_failed", err.Error(), tokens
			}
			// 摘要口径单一真源：tool 消息同样推进 last_message_at。
			// 此前只在 assistant 消息处 Touch，导致 sessions.last_message_at
			// 停在 assistant（事故会话：停在 seq106）而 updated_at 已到 seq108。
			if e.deps.Summary != nil {
				_ = e.deps.Summary.TouchMessage(ctx, req.SessionID, e.deps.Now())
			}
			history = append(history, toLLMMessage(toolMsg))
			if cancelled {
				return model.RunCancelled, "context_canceled", "", "", tokens
			}
		}
		userInput = "" // 后续轮次不再重复用户输入
	}

	return model.RunFailed, "", "max_steps_exceeded",
		fmt.Sprintf("超过最大步数 %d", e.deps.MaxSteps), tokens
}

// drainGuidance 消费运行中引导（轮次边界）：消息已由 service 层落库，这里只
// 回灌上下文（history）并广播 message.appended 事件供多端对齐。非阻塞：无引导立即返回。
func (e *Engine) drainGuidance(ctx context.Context, sdb SessionDB, req RunRequest, history *[]llm.Message) {
	if req.GuidanceCh == nil {
		return
	}
	for {
		select {
		case msg := <-req.GuidanceCh:
			if msg == nil || strings.TrimSpace(msg.Content) == "" {
				continue
			}
			*history = append(*history, toLLMMessage(msg))
			if e.deps.Summary != nil {
				_ = e.deps.Summary.TouchMessage(ctx, req.SessionID, e.deps.Now())
			}
			e.publish(req.RunID, events.TypeMessageAppended, map[string]any{
				"message_id": msg.ID,
				"role":       string(model.RoleUser),
				"content":    msg.Content,
				"seq":        msg.Seq,
			})
		default:
			return
		}
	}
}

// streamOutcome 单轮流式结果。
type streamOutcome struct {
	content   string
	reasoning string
	toolCalls []llm.ToolCall
	// messageID 本轮 assistant 消息 ID（审计日志按它关联「这次调用产出哪条消息」）。
	messageID string
	// cachedTokens 本轮缓存命中 token（runs 表 cached_tokens 的落点）。
	cachedTokens int64
	// estimated 本轮用量是否为估算值（runs 表 is_estimated 的落点）。
	// 绝不隐藏估算值：任一段 usage 被标记估算，整次调用即记 1。
	estimated bool
	// promptTokens / completionTokens 本轮流式用量（P1：随终态回写 runs 表，
	// OpenAI 兼容端点与 UI 的 usage 以此为准）。
	promptTokens     int64
	completionTokens int64
	// durationMs 本轮流式调用耗时（startedAt → 返回时刻），与审计 trace 的
	// DurationMs 同源。随 message.end 下发、随 usage_json 落库：
	// 前端原先只能用本地计时器（刷新即丢），历史回放时整轮耗时/步耗时全为空。
	durationMs int64
}

// messageUsageJSON 组装 assistant 消息 usage_json 的载荷。
//
// 只写四元组（prompt_tokens / completion_tokens / cached_tokens / duration_ms），
// **不写 Provider 原始 usage 整包**：不同端点的 usage 形状不一致（有的带
// prompt_tokens_details、有的只给 total），落库后回放解析会各自漂移。
// 无任何用量（上游未回 usage）时返回 nil —— 让 DB 留空，前端据此降级为不展示，
// 绝不用 0 冒充「真实测得 0」。
func messageUsageJSON(out streamOutcome) json.RawMessage {
	if out.promptTokens == 0 && out.completionTokens == 0 && out.cachedTokens == 0 {
		return nil
	}
	raw, err := json.Marshal(map[string]int64{
		"prompt_tokens":     out.promptTokens,
		"completion_tokens": out.completionTokens,
		"cached_tokens":     out.cachedTokens,
		"duration_ms":       out.durationMs,
	})
	if err != nil {
		return nil
	}
	return raw
}

// streamOnce 执行一轮模型调用并消费流。
// 返回的 status 非空表示异常终态（cancelled/failed）。
func (e *Engine) streamOnce(ctx context.Context, req RunRequest, provider llm.Provider, res prompt.BuildResult) (
	out streamOutcome, status model.RunStatus, errCode, errMsg string, tokens int64,
) {
	// 审计埋点：仅当注入了 Traces 才收集（nil 时零开销，行为与之前完全一致）。
	startedAt := e.now()
	firstTokenAt := int64(0)
	var payloadJSON string
	if e.deps.Traces != nil {
		payloadJSON = marshalForTrace(res.Request)
	}

	stream, err := provider.ChatStream(ctx, res.Request)
	if err != nil {
		if ctx.Err() != nil {
			e.recordLLMTrace(ctx, req, provider, res, startedAt, firstTokenAt, payloadJSON, out, tokens, "cancelled", "context_canceled", "")
			return out, model.RunCancelled, "context_canceled", "", 0
		}
		le := llm.AsError(err)
		e.recordLLMTrace(ctx, req, provider, res, startedAt, firstTokenAt, payloadJSON, out, tokens, "error", string(le.Kind), le.Message)
		return out, model.RunFailed, string(le.Kind), le.Message, 0
	}
	defer func() { _ = stream.Close() }()

	var content strings.Builder
	var reasoning strings.Builder
	msgID := newID()
	e.publish(req.RunID, events.TypeMessageStart, map[string]any{
		"message_id": msgID,
		"role":       string(model.RoleAssistant),
	})

	type acc struct {
		id   string
		name string
		args strings.Builder
	}
	accs := make(map[int]*acc)

	for {
		ch, rErr := stream.Recv()
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			if ctx.Err() != nil {
				e.recordLLMTrace(ctx, req, provider, res, startedAt, firstTokenAt, payloadJSON, out, tokens, "cancelled", "context_canceled", "")
				return out, model.RunCancelled, "context_canceled", "", tokens
			}
			le := llm.AsError(rErr)
			e.recordLLMTrace(ctx, req, provider, res, startedAt, firstTokenAt, payloadJSON, out, tokens, "error", string(le.Kind), le.Message)
			return out, model.RunFailed, string(le.Kind), le.Message, tokens
		}
		// 首次收到任何增量（正文/思考/工具调用）即记 TTFT
		if firstTokenAt == 0 && (ch.Delta != "" || ch.Reasoning != "" || ch.ToolCall != nil) {
			firstTokenAt = e.now()
		}
		if ch.Delta != "" {
			content.WriteString(ch.Delta)
			// delta 只发事件，不落库（ADR-003）
			e.publish(req.RunID, events.TypeMessageDelta, map[string]any{
				"message_id": msgID,
				"delta":      ch.Delta,
			})
		}
		if ch.Reasoning != "" {
			// 思考过程增量：与 delta 同语义只发事件不落库（最终整段落库于 assistant 消息）
			reasoning.WriteString(ch.Reasoning)
			e.publish(req.RunID, events.TypeMessageReasoning, map[string]any{
				"message_id": msgID,
				"delta":      ch.Reasoning,
			})
		}
		if ch.Usage != nil {
			tokens += int64(ch.Usage.TotalTokens)
			out.promptTokens += int64(ch.Usage.PromptTokens)
			out.completionTokens += int64(ch.Usage.CompletionTokens)
			out.cachedTokens += int64(ch.Usage.CachedTokens)
			// 任一段 usage 被标记为估算值，则整次调用按估算记账（不掩盖）
			if ch.Usage.Estimated {
				out.estimated = true
			}
		}
		if ch.ToolCall != nil {
			a, ok := accs[ch.ToolCall.Index]
			if !ok {
				a = &acc{}
				accs[ch.ToolCall.Index] = a
			}
			if ch.ToolCall.ID != "" {
				a.id = ch.ToolCall.ID
			}
			if ch.ToolCall.Name != "" {
				a.name = ch.ToolCall.Name
			}
			a.args.WriteString(ch.ToolCall.ArgumentsDelta)
		}
	}

	out.content = content.String()
	out.reasoning = reasoning.String()
	out.messageID = msgID
	// 本轮耗时：与审计 trace 的 duration 同源（streamOnce 入口 startedAt → 此刻）。
	// 必须记在「流读完」这个点，而不是工具执行之后 —— 否则会把工具耗时算进
	// 模型调用耗时，前端步耗时与审计日志对不上。
	if d := e.now() - startedAt; d > 0 {
		out.durationMs = d
	}
	// 按 index 排序输出，保证多工具调用的稳定顺序
	indexes := make([]int, 0, len(accs))
	for i := range accs {
		indexes = append(indexes, i)
	}
	sortInts(indexes)
	for _, i := range indexes {
		a := accs[i]
		out.toolCalls = append(out.toolCalls, llm.ToolCall{
			ID:        a.id,
			Name:      a.name,
			Arguments: json.RawMessage(a.args.String()),
		})
	}
	e.recordLLMTrace(ctx, req, provider, res, startedAt, firstTokenAt, payloadJSON, out, tokens, "ok", "", "")
	return out, "", "", "", tokens
}

// effectiveAccessMode 返回当前审批应采用的访问模式：
// Deps.AccessModeProvider 可用时实时读库（切换即时生效），失败回落 Run 启动快照。
func (e *Engine) effectiveAccessMode(ctx context.Context, req RunRequest) approval.AccessMode {
	if e.deps.AccessModeProvider == nil {
		return req.AccessMode
	}
	mode, err := e.deps.AccessModeProvider(ctx, req.SessionID)
	if err != nil || mode == "" {
		return req.AccessMode
	}
	return mode
}

// approvalRules 取本次审批生效的规则集。
//
// 优先级：Deps.Rules（会话级 + 项目级合并）> 会话库规则（旧行为）。
// 容错语义与原来完全一致：读取失败一律视作「无规则」——结果是更保守的逐次审批，
// 绝不因规则表抖动而让 Run 失败。
func (e *Engine) approvalRules(ctx context.Context, sdb SessionDB, req RunRequest) []approval.Rule {
	if e.deps.Rules != nil {
		if rules, err := e.deps.Rules.RulesFor(ctx, req.SessionID); err == nil {
			return rules
		}
	}
	rules, _ := sdb.ApprovalRules(ctx)
	return rules
}

// toolDurationMs 工具执行耗时（毫秒，非负）。
//
// 口径必须与 tool_events 落库的 ended_at - started_at 完全一致，否则「实时流
// 看到的耗时」与「刷新后历史查到的耗时」会打架。store 层 Finish 用同一时钟取
// ended_at，这里直接用同一时基做减法即可（负数一律归零，时钟回拨不产生负耗时）。
func (e *Engine) toolDurationMs(startedAt int64) int64 {
	if d := e.deps.Now() - startedAt; d > 0 {
		return d
	}
	return 0
}

// execTool 执行单次工具调用（含审批）。返回的 cancelled 表示被中断。
func (e *Engine) execTool(ctx context.Context, sdb SessionDB, req RunRequest, tc llm.ToolCall) (
	msg *model.Message, cancelled bool,
) {
	eventID := newID()
	// 风险级由工具契约提供（Catalog 未注入时按 write 保守处理）；
	// RiskOverride 可按参数升级（高危命令特征检测 → danger，full 模式仍逐次审批）
	risk := model.RiskWrite
	if e.deps.Catalog != nil {
		if r, ok := e.deps.Catalog.Risk(tc.Name); ok {
			risk = r
		}
	}
	if e.deps.RiskOverride != nil {
		risk = e.deps.RiskOverride(tc.Name, tc.Arguments, risk)
	}
	ev := &model.ToolEvent{
		ID:          eventID,
		RunID:       req.RunID,
		ToolName:    tc.Name,
		Namespace:   "builtin",
		ArgsJSON:    normalizeArgs(tc.Arguments),
		Status:      model.ToolPending,
		Risk:        risk,
		Executor:    model.ExecutorServer,
		SourceTrust: model.TrustTrusted,
	}
	// localExec 未执行或以本地为准的路径（审批拒绝/取消/前置失败）一律 server + trusted；
	// 真正执行后的执行面归属在 Finish 阶段按真实结果回写（见下方 result）。
	localExec := model.ToolEventExec{Executor: model.ExecutorServer, SourceTrust: model.TrustTrusted}
	startedAt := e.deps.Now()
	ev.StartedAt = &startedAt
	if err := sdb.InsertToolEvent(dbctx(ctx), ev); err != nil {
		return toolMessage(tc, "工具事件落库失败: "+err.Error()), false
	}
	e.publish(req.RunID, events.TypeToolStarted, map[string]any{
		"tool_event_id": eventID,
		"tool_name":     tc.Name,
		"risk":          string(risk),
		// args 必须与「开始」一起下发（协议 §tool.started payload 本就含 args）。
		// 此前漏发，前端工具卡片 payload.args 恒为空对象 → 命令类工具（run_command）在执行中
		// 只显示流式输出、看不到「当前跑的是什么命令」（命令行是 cmd+args 拼出来的，
		// 后端不下发 cmdline，前端无从拼装）。与落库 ev.ArgsJSON 同源（都用 normalizeArgs），
		// 保证「历史回看」与「实时流」看到的命令行完全一致。
		"args": normalizeArgs(tc.Arguments),
	})

	// ===== 能力天花板短路（协议 §4.1 / §4.2）=====
	//
	// 必须按 ToolPolicy 三态裁决，且必须在 Gate.Evaluate **之前**短路拒绝，理由：
	//   ① 若走审批路径，会滞留 waiting_approval，而 DefaultTimeout = 5min（超时才视为拒绝），
	//      用户要白等 5 分钟才被告知"不行"；
	//   ② 状态机会经历一次无意义的 waiting_approval 迁移，污染 runs.status 与下游重建逻辑；
	//   ③ 这是能力边界而非人工决策，本就不该交给人来点。
	// 拒绝粒度是**工具调用级**：Run 必须继续执行，用户可继续聊别的话题。
	//
	// 🚨 历史缺陷（勿回退）：此处曾用 `DenyTools bool` 只拦 command + danger，
	// 导致 write 级（write_file / edit_file）被静默放行 —— 移动端「禁止文件写入」
	// 与无设备空间「零工具」两条契约同时失效。改三态后由 Allows() 统一裁决，
	// 新增风险级只需改一处。
	if !req.ToolPolicy.Allows(risk) {
		reason := req.PolicyReason
		if strings.TrimSpace(reason) == "" {
			reason = defaultPolicyReason(req.ToolPolicy)
		}
		// 落库为 denied（审计可查），并保留 risk 便于回溯。
		_ = sdb.FinishToolEvent(dbctx(ctx), eventID, model.ToolDenied, "", "", reason, localExec)
		e.publish(req.RunID, events.TypeToolResult, map[string]any{
			"tool_event_id": eventID,
			"tool_name":     tc.Name,
			"status":        string(model.ToolDenied),
			"result":        reason,
			"duration_ms":   e.toolDurationMs(startedAt),
		})
		return toolMessage(tc, reason), false
	}

	// 审批规则：Deps.Rules 非 nil 时由它统一提供「会话级 + 项目级」合并集，
	// 否则退回会话库规则（旧行为）。合并策略在 service.ApprovalPolicy 中唯一实现，
	// 引擎只消费结果，不做第二套判定。
	rules := e.approvalRules(ctx, sdb, req)
	// 跨端仲裁（C-7）：发起端 = 本次 Run 的执行端设备（空串 = 服务端本地执行面）。
	requester := approvalRequester(req)
	// 高风险 + 端侧发起 → 绑定「必须由该设备确认」（strict_approval_device 决定是否强制）。
	required := ""
	if requester != "" && highRiskNeedsDevice(risk) {
		required = requester
	}
	verdict := e.deps.Gate.Evaluate(approval.Request{
		ApprovalID:        eventID,
		RunID:             req.RunID,
		ToolEventID:       eventID,
		ToolName:          tc.Name,
		Risk:              risk,
		ArgsJSON:          string(tc.Arguments),
		AccessMode:        e.effectiveAccessMode(ctx, req),
		Rules:             rules,
		RequestedByDevice: requester,
		RequiredDeviceID:  required,
	})

	decision := verdict.Auto
	reason := verdict.Reason
	if verdict.NeedApproval {
		if _, err := e.deps.Gate.RegisterWithDevice(eventID, requester, required); err != nil {
			_ = sdb.FinishToolEvent(dbctx(ctx), eventID, model.ToolError, "", "", err.Error(), localExec)
			return toolMessage(tc, "审批登记失败: "+err.Error()), false
		}
		// 状态机闭环（单一真相铁律）：审批等待**必须**落到 runs.status=waiting_approval。
		//
		// 此前这里只发 approval.request 事件就阻塞在 Gate.Await，从不迁移状态，
		// 导致三处下游全部失真（历史缺陷，勿回退）：
		//   ① DecideApproval 硬校验 run.Status==waiting_approval → 用户点「允许」永远 409；
		//   ② GET /active-run 的 status 永不为 waiting_approval → 前端「刷新后重建审批条」是死代码；
		//   ③ 跨端/审计看到的是 running，无法区分「在跑」与「在等人」。
		// 迁移失败**必须硬失败**（绝不能静默降级回旧行为）：否则又回到「看得见审批、点不动」的死链。
		if err := e.transition(ctx, sdb, req.RunID, model.RunWaitingApproval, "", "", ""); err != nil {
			_ = sdb.FinishToolEvent(dbctx(ctx), eventID, model.ToolError, "", "", err.Error(), localExec)
			return toolMessage(tc, "审批状态迁移失败: "+err.Error()), false
		}
		e.publishStatus(req.RunID, req.SessionID, model.RunWaitingApproval, "", "", "", nil)
		e.publish(req.RunID, events.TypeApprovalRequest, map[string]any{
			"approval_id":   eventID,
			"tool_event_id": eventID,
			"tool_name":     tc.Name,
			"risk":          string(risk),
			"reason":        reason,
			"session_id":    req.SessionID,
			// run_id / args：前端审批条要显示「哪个 Run、放行什么参数」，
			// 刷新恢复时也要靠 run_id 决定决策提交目标（此前缺这两个字段，前端读出来恒为空）。
			"run_id": req.RunID,
			"args":   string(normalizeArgs(tc.Arguments)),
			// 新增字段（协议向后兼容，仅新增不改形状）：前端据此显示「需在端侧确认」
			"requested_by_device": requester,
			"required_device_id":  required,
		})
		d, r := e.deps.Gate.Await(ctx, eventID)
		decision = d
		if r != "" {
			reason = r
		}
		// 无论批准/拒绝/超时，闸门已解除 → 状态回到 running（Run 仍在跑，拒绝原因随后回灌模型）。
		// 取消路径**不回迁**：让状态停在 waiting_approval，由上层收口为 cancelled
		// （state.go 允许 waiting_approval → cancelled），避免与取消语义打架。
		if ctx.Err() == nil {
			if err := e.transition(ctx, sdb, req.RunID, model.RunRunning, "", "", ""); err != nil {
				_ = sdb.FinishToolEvent(dbctx(ctx), eventID, model.ToolError, "", "", err.Error(), localExec)
				return toolMessage(tc, "审批后状态回迁失败: "+err.Error()), false
			}
			e.publishStatus(req.RunID, req.SessionID, model.RunRunning, "", "", "", nil)
		}
		e.publish(req.RunID, events.TypeApprovalDecision, map[string]any{
			"approval_id":         eventID,
			"decision":            string(decision),
			"reason":              reason,
			"requested_by_device": requester,
			"required_device_id":  required,
		})
		if ctx.Err() != nil {
			_ = sdb.FinishToolEvent(dbctx(ctx), eventID, model.ToolDenied, "", "", "Run 已取消", localExec)
			return toolMessage(tc, "denied: Run 已取消"), true
		}
	}

	if decision == approval.Deny {
		// 拒绝原因回传模型，让它换方案继续（不中断 Run）
		_ = sdb.FinishToolEvent(dbctx(ctx), eventID, model.ToolDenied, "", "", reason, localExec)
		e.publish(req.RunID, events.TypeToolResult, map[string]any{
			"tool_event_id": eventID,
			"status":        string(model.ToolDenied),
			"error":         reason,
			"duration_ms":   e.toolDurationMs(startedAt),
		})
		return toolMessage(tc, "denied: "+reason), false
	}
	if decision == approval.AllowSession {
		_ = sdb.AddApprovalRule(ctx, tc.Name, "")
	}

	// 执行
	if e.deps.Tools == nil {
		const msg = "工具执行器未注册（A 阶段仅协议骨架）"
		_ = sdb.FinishToolEvent(dbctx(ctx), eventID, model.ToolError, "", "", msg, localExec)
		e.publish(req.RunID, events.TypeToolResult, map[string]any{
			"tool_event_id": eventID,
			"status":        string(model.ToolError),
			"error":         msg,
			"duration_ms":   e.toolDurationMs(startedAt),
		})
		return toolMessage(tc, msg), false
	}

	toolCtx, cancel := context.WithTimeout(ctx, e.deps.ToolTimeout)
	defer cancel()
	effMode := e.effectiveAccessMode(ctx, req)
	result, runErr := e.deps.Tools.Run(toolCtx, ToolRunRequest{
		RunID:       req.RunID,
		SessionID:   req.SessionID,
		ToolEventID: eventID,
		ToolName:    tc.Name,
		ArgsJSON:    string(tc.Arguments),
		AccessMode:  string(effMode),
		// 执行面归属（D7）：显式指定设备，禁止派发器自行挑选其他在线设备。
		DeviceID: req.ExecutionDeviceID,
	})
	if runErr != nil {
		errText := runErr.Error()
		_ = sdb.FinishToolEvent(dbctx(ctx), eventID, model.ToolError, "", "", errText, localExec)
		e.publish(req.RunID, events.TypeToolResult, map[string]any{
			"tool_event_id": eventID,
			"status":        string(model.ToolError),
			"error":         errText,
			"duration_ms":   e.toolDurationMs(startedAt),
		})
		return toolMessage(tc, "工具执行失败: "+errText), false
	}

	// 执行面归属以**实际执行结果**为准（C-5 多端协同 HALO）：
	//   - 端侧执行 → executor=device / device_id=<id> / source_trust=untrusted（不变量 4）
	//   - 静默降级 → executor=server / source_trust=trusted（不变量 5，不向用户报错）
	// 零值由 Normalized 兜底为 server + trusted，保证未接设备域时行为不变。
	execInfo := model.ToolEventExec{
		Executor:    result.Executor,
		DeviceID:    result.DeviceID,
		SourceTrust: result.SourceTrust,
	}.Normalized()
	_ = sdb.FinishToolEvent(dbctx(ctx), eventID, result.Status, result.ResultJSON, result.ResultBlob,
		result.Error, execInfo)
	// 审计副本（天库）：会话库的 tool_events 是事实源，这里额外投递一条可丢弃的查询副本。
	// 丢失只影响跨会话按时间审计，不影响任何业务功能。
	e.recordToolTrace(ctx, req, ev, result.ResultJSON, result.ResultBlob, result.Error, execInfo, result.Status)
	e.publish(req.RunID, events.TypeToolResult, map[string]any{
		"tool_event_id": eventID,
		"status":        string(result.Status),
		"result":        result.ResultJSON,
		"error":         result.Error,
		"session_id":    req.SessionID,
		// duration_ms 工具执行耗时（ended_at - started_at，与 tool_events 落库同源）。
		// 此前不下发，前端 mapSseToStreamEvent 只能写死 0 → 工具卡片的
		// 「xx ms」徽标在实时流里永不出现（原型 ToolCard 是显示的），
		// 只有刷新后从 tool-events 历史查才有值。此处按同一口径补齐。
		"duration_ms": e.toolDurationMs(startedAt),
		// 新增字段（协议向后兼容，仅新增不改形状）：前端可据此显示「端侧执行」徽标
		"executor":     string(execInfo.Executor),
		"device_id":    execInfo.DeviceID,
		"source_trust": string(execInfo.SourceTrust),
	})
	payload := result.ResultJSON
	if payload == "" {
		payload = result.Error
	}
	return toolMessage(tc, payload), false
}

// tryCompact 尝试执行一次上下文压缩。
//
// 返回 (新历史, 是否真的压缩成功)。失败一律返回 (nil, false)，
// 调用方继续走原有的 budget_exhausted 路径 —— 压缩绝不能把
// 「超预算」这个明确失败变成「压缩失败导致的未知状态」。
//
// # 为什么这里要把 messages 全量加载
//
// 压缩决策需要「轮次边界」（最后一条 user 在哪里），这只能从完整消息序列判断。
// 引擎的 history 是 llm.Message（已丢失 seq 与 hidden），不足以做边界判断。
func (e *Engine) tryCompact(
	ctx context.Context, sdb SessionDB, req RunRequest, built prompt.BuildResult, history []llm.Message,
) ([]llm.Message, bool) {
	// 入口守卫：压缩是可选能力，未装配时必须安全短路。
	// 调用点虽有判定，但把守卫放在函数内部才能真正防住"新增调用点忘记判断"。
	if e.deps.Compactor == nil {
		return nil, false
	}
	cctx, cancel := detachedCtx(ctx)
	defer cancel()

	msgs, err := sdb.ListMessages(cctx, 0, 1000)
	if err != nil {
		return nil, false
	}
	res, err := e.deps.Compactor.Compact(cctx, CompactRequest{
		SessionID: req.SessionID,
		ModelRef:  req.ModelRef,
		Used:      built.Breakdown.Total,
		Limit:     e.deps.MaxContextTokens,
		Messages:  msgs,
		Running:   true, // 本 Run 正在执行：必须保护当前轮次
	})
	if err != nil || !res.Compacted {
		return nil, false
	}
	// 压缩成功：重建历史（已压缩区间由摘要替代）。
	newHistory, err := e.deps.Compactor.History(cctx, req.SessionID, msgs)
	if err != nil {
		return nil, false
	}
	// 让压缩对用户与前端**可见**：否则用户只看到"突然变快了"，
	// 而模型却"忘掉了"部分历史，排障时无从下手。
	e.publish(req.RunID, events.TypeError, map[string]any{
		"scope": "compaction",
		"code":  "agent.context_compacted",
		"message": fmt.Sprintf("上下文已达水位，已将第 %d–%d 条消息（%d 条）折叠为摘要，估算节省 %d tokens",
			res.StartSeq, res.EndSeq, res.ReplacedMessages, res.SavedTokens),
		"start_seq":         res.StartSeq,
		"end_seq":           res.EndSeq,
		"replaced_messages": res.ReplacedMessages,
		"saved_tokens":      res.SavedTokens,
	})
	return newHistory, true
}

// stepNotice 生成步数进度提示文本（走 prompt.Input.DynamicTail 通道）。
//
// 设计要点：
//   - step 从 0 计，对外展示一律用 step+1（即「当前第几步」）；
//   - 每 every 步注入一次（第 every、2×every … 步），every <= 0 表示关闭；
//   - 最后一步额外追加收敛指令：此时已无余量继续调用工具，必须强制模型
//     交出最终结论，避免像以往那样被硬截断后「无声停住」。
//
// 返回空串时 Builder 不会追加多余空行（joinTails 已过滤空段）。
func stepNotice(step, maxSteps, every int) string {
	if every <= 0 || maxSteps <= 0 {
		return ""
	}
	current := step + 1

	// 最后一步优先级最高：无论是否命中 every 的整数倍，都必须提示收敛。
	if current >= maxSteps {
		return fmt.Sprintf("【步数进度】当前步数 %d/总步数 %d。已到最后一步，"+
			"后续无法再调用任何工具，请立即基于已有信息给出最终答复；"+
			"若任务未完成，请说明已完成部分、剩余部分与建议的下一步。",
			current, maxSteps)
	}

	if current%every != 0 {
		return ""
	}
	return fmt.Sprintf("【步数进度】当前步数 %d/总步数 %d，剩余 %d 步。"+
		"请据此规划剩余工作，优先保证能给出完整结论。",
		current, maxSteps, maxSteps-current)
}

// degradeDetails 提取降级动作明细（事件 payload 用）。
func degradeDetails(actions []prompt.DegradeAction) []string {
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		if a.Detail != "" {
			out = append(out, prompt.LevelName(a.Level)+"："+a.Detail)
		}
	}
	return out
}

// loadHistory 载入会话历史并转成 llm.Message。
func (e *Engine) loadHistory(ctx context.Context, sdb SessionDB) ([]llm.Message, error) {
	msgs, err := sdb.ListMessages(ctx, 0, 1000)
	if err != nil {
		return nil, err
	}
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, toLLMMessage(&m))
	}
	// 兼容历史脏数据：早期版本在 tool_calls 序列化失败时会丢掉 assistant 的声明，
	// 却仍把 tool 结果落库，于是库里留下「tool_call_id 无人声明」的孤儿 tool 消息。
	// 这类消息一旦被重放，上游必然 400（协议要求 tool 消息紧跟声明它的 assistant）。
	// 已在写入侧修复（见 marshalToolCalls），但**老会话仍需在此过滤才能恢复可用**。
	return dropOrphanToolMessages(out), nil
}

// dropOrphanToolMessages 丢弃没有对应 assistant 声明的 tool 消息。
//
// 协议口径（OpenAI Chat Completions）：
//   - 每条 role=tool 的消息必须通过 tool_call_id 指向**紧邻之前**某个 assistant 的 tool_calls；
//   - 未被应答的 tool_call 同样违规，因此配对失败时连声明一起丢弃最稳妥。
//
// 实现按序扫描：遇到 assistant 时把它的 tool_call id 记入待答集合，
// 遇到 tool 且能在待答集合中命中才算合法并消费掉；否则丢弃。
// 这样「重复应答」「顺序错乱」「凭空出现」三种脏数据都能被清掉。
func dropOrphanToolMessages(in []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(in))
	pending := make(map[string]struct{}) // 当前 assistant 声明过、尚未被应答的 tool_call id
	dropped := 0
	for _, m := range in {
		switch m.Role {
		case llm.RoleAssistant:
			pending = make(map[string]struct{}, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					pending[tc.ID] = struct{}{}
				}
			}
			out = append(out, m)
		case llm.RoleTool:
			if _, ok := pending[m.ToolCallID]; ok {
				delete(pending, m.ToolCallID)
				out = append(out, m)
				continue
			}
			// 孤儿 tool 消息：丢弃（不重放，避免上游 400）
			dropped++
		default:
			// user / system 等出现即视为一轮新输入，之前未应答的 tool_call 不再需要配对
			pending = make(map[string]struct{})
			out = append(out, m)
		}
	}
	if dropped > 0 {
		// 必须留痕：说明该会话历史里存在脏数据（写入侧修复前遗留）。
		slog.WarnContext(context.Background(), "历史中存在孤儿 tool 消息，已丢弃以保请求合法",
			"dropped", dropped, "kept", len(out), "total", len(in))
	}
	return out
}

func (e *Engine) transition(ctx context.Context, sdb SessionDB, runID string, to model.RunStatus, stopReason, errCode, errMsg string) error {
	return sdb.TransitionRun(ctx, runID, to, stopReason, errCode, errMsg)
}

func (e *Engine) publish(runID string, typ events.Type, payload any) {
	// 事件发布失败不应影响 Run（预览通道，DB 才是权威）
	_, _ = e.deps.Events.Publish(runID, typ, payload)
}

func (e *Engine) publishStatus(runID, sessionID string, status model.RunStatus, stopReason, errCode, errMsg string, usage *runUsage) {
	payload := map[string]any{"status": string(status), "session_id": sessionID}
	if stopReason != "" {
		payload["stop_reason"] = stopReason
	}
	if errCode != "" {
		payload["error_code"] = errCode
	}
	if errMsg != "" {
		payload["error_message"] = errMsg
	}
	// 终态随行 usage（P1：与 runs 表 AddRunUsage 同源同值；前端用量条/上下文水位以此为准）。
	// running 状态 usage 为 nil（尚未产生），不出栈这两个字段，保持 payload 形状向后兼容。
	if usage != nil {
		payload["input_tokens"] = usage.prompt
		payload["output_tokens"] = usage.completion
		// cached_tokens 必须下发：前端会话账本（usageBySession）的缓存命中率
		// 分母是输入、分子是它。此前只给 input/output，前端 `e.cachedTokens` 恒
		// undefined → 累计命中数恒 0 → Composer 与 ContextGauge 的「缓存命中」
		// 一行永远不渲染（不是命中率为 0，而是整行消失）。
		payload["cached_tokens"] = usage.cached
		// is_estimated 一并透出：让 UI 能如实标注「估算值」而不是伪装成真实测量。
		payload["is_estimated"] = usage.estimated
	}
	e.publish(runID, events.TypeRunStatus, payload)
}

func (e *Engine) lockSession(sessionID string) func() {
	v, _ := e.locks.LoadOrStore(sessionID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// leaseHolder 取本次 Run 的租约持有者（空串 = 服务端本地执行面）。
//
// 采用空串而非 "server"：与 model.SessionLease.DeviceID 的既有语义严格一致
// （store 层用「device_id 空串 = 服务端本地持有」做等值比较与变更判定）。
func leaseHolder(req RunRequest) string {
	return strings.TrimSpace(req.LeaseHolder)
}

// approvalRequester 取审批发起端设备 ID（空串 = 服务端本地执行面）。
//
// 优先 RequesterDevice（显式声明），未填时回退 LeaseHolder：
// 端侧发起 Run 时两者通常一致，回退保证未改造的调用点仍能带上跨端审计信息。
func approvalRequester(req RunRequest) string {
	if v := strings.TrimSpace(req.RequesterDevice); v != "" {
		return v
	}
	return leaseHolder(req)
}

// highRiskNeedsDevice 判定该风险级是否属「高风险」——跨端仲裁要求执行端确认。
//
// 口径对齐多端协同 §5.4：仅 danger（exec/high 归入此类）强制在执行端确认一次；
// write/command 仍走常规审批（不绑定设备，避免把所有写操作锁死在端侧）。
func highRiskNeedsDevice(risk model.Risk) bool {
	return risk == model.RiskDanger
}

// publishLeaseWarn 租约异常只作可观测信号推给事件流（不改变 Run 终态）。
//
// 铁律：租约是**协调信息**，不是业务正确性来源。单进程内正确性由本地 mutex 保证，
// 因此租约失败绝不阻断 Run（否则主库抖动会让所有对话不可用）。
// 事件类型用非致命 TypeError（Run 仍可继续），payload 带 scope 便于前端区分来源。
func (e *Engine) publishLeaseWarn(runID, reason string, err error) {
	e.publish(runID, events.TypeError, map[string]any{
		"scope":   "session_lease",
		"code":    "agent.lease_" + reason,
		"message": err.Error(),
	})
}

// renewLease 续期会话租约（每步迭代调用；nil 端口时无操作）。
func (e *Engine) renewLease(ctx context.Context, req RunRequest) {
	if e.deps.Leases == nil {
		return
	}
	if err := e.deps.Leases.Renew(ctx, req.SessionID, leaseHolder(req)); err != nil {
		e.publishLeaseWarn(req.RunID, "renew_failed", err)
	}
}

// toLLMMessage 把存储层消息转成模型消息。
func toLLMMessage(m *model.Message) llm.Message {
	out := llm.Message{
		Role:       llm.Role(m.Role),
		Content:    m.Content,
		ToolCallID: m.ToolCallID,
	}
	if len(m.ToolCallsJSON) > 0 {
		var tcs []llm.ToolCall
		if err := json.Unmarshal(m.ToolCallsJSON, &tcs); err == nil {
			out.ToolCalls = tcs
		}
	}
	return out
}

// normalizeArgs 保证落库的 args 是合法 JSON。
//
// tool_events.args 是 json.RawMessage：模型偶尔给出非法 JSON，
// 直接落库会让整个事件序列化失败（曾导致 /tool-events 返回空响应体）。
func normalizeArgs(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	if json.Valid(raw) {
		return raw
	}
	b, err := json.Marshal(string(raw))
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// marshalToolCalls 序列化 assistant 的 tool_calls 供落库，**永不返回 nil**。
//
// 为什么必须保证非 nil（真实事故，会话被永久污染）：
// llm.ToolCall.Arguments 是 json.RawMessage，一旦模型给出的参数不是合法 JSON
// （空串、流式被截断成 "["、畸形对象），json.Marshal 会整段失败。
// 旧写法 `if raw, err := json.Marshal(...); err == nil { ... }` 把失败静默吞掉，
// 于是 assistant 消息丢掉了 tool_calls 声明，但引擎仍照常执行工具并把 tool 结果落库，
// 历史中便留下「tool_call_id 无任何 assistant 声明过」的孤儿 tool 消息。
// 下一次请求重放该历史时，OpenAI 兼容上游直接 400（bad_request）。
//
// 修法对齐老版本 harness_mini：那边 arguments 存 JSON 字符串，天然不会序列化失败；
// 这里保持 Arguments 为 RawMessage（与流式解析一致），但在落库前把非法参数归一为
// 合法 JSON，确保「声明与结果永远成对」。
//
// 归一策略与 normalizeArgs 一致：非法时先尝试包成 JSON 字符串保留原文，
// 连字符串化都失败则退化为 {}。宁可参数降级，也不能丢声明。
func marshalToolCalls(e *Engine, req RunRequest, tcs []llm.ToolCall) json.RawMessage {
	safe := make([]llm.ToolCall, 0, len(tcs))
	sanitized := 0
	for _, tc := range tcs {
		args := normalizeArgs(tc.Arguments)
		if string(args) != string(tc.Arguments) {
			sanitized++
		}
		safe = append(safe, llm.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: args})
	}
	if sanitized > 0 {
		// 参数被降级必须留痕：模型反复吐畸形参数是上游/提示词问题的信号。
		slog.WarnContext(context.Background(), "工具调用参数非法，已归一化以保全 tool_calls 声明",
			"session_id", req.SessionID,
			"run_id", req.RunID,
			"sanitized", sanitized,
			"total", len(tcs),
		)
	}
	raw, err := json.Marshal(safe)
	if err != nil {
		// 归一后理论上不可达；兜底仍返回空数组而非 nil，避免声明整段消失。
		slog.ErrorContext(context.Background(), "tool_calls 序列化失败，已降级为空数组",
			"session_id", req.SessionID, "run_id", req.RunID, "err", err)
		return json.RawMessage(`[]`)
	}
	return raw
}

// toolMessage 构造回灌给模型的 tool 消息。
func toolMessage(tc llm.ToolCall, content string) *model.Message {
	return &model.Message{
		ID:         newID(),
		Role:       model.RoleTool,
		Content:    content,
		ToolCallID: tc.ID,
		CreatedAt:  time.Now().UnixMilli(),
	}
}

// newID 生成 16 字节随机 ID。
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("id-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func sortInts(s []int) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
