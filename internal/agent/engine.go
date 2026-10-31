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
	ToolTimeout   time.Duration
	Now           func() int64
}

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
		deps.MaxSteps = 20
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

	sdb, err := e.deps.Sessions.Session(ctx, req.SessionID)
	if err != nil {
		return fmt.Errorf("agent: 打开会话库失败: %w", err)
	}

	run, err := sdb.GetRun(ctx, req.RunID)
	if err != nil {
		return fmt.Errorf("agent: 读取 Run 失败: %w", err)
	}
	// 状态机校验（单点收口）
	if err := CheckTransition(run.Status, model.RunRunning); err != nil {
		return err
	}
	if err := e.transition(ctx, sdb, req.RunID, model.RunRunning, "", "", ""); err != nil {
		return err
	}
	e.publishStatus(req.RunID, req.SessionID, model.RunRunning, "", "", "")

	final, stopReason, errCode, errMsg, tokens := e.loop(ctx, sdb, req)

	if err := e.transition(ctx, sdb, req.RunID, final, stopReason, errCode, errMsg); err != nil {
		// 终态迁移失败不掩盖原始错误
		return fmt.Errorf("agent: 终态迁移失败: %w (原始状态=%s)", err, final)
	}
	e.publishStatus(req.RunID, req.SessionID, final, stopReason, errCode, errMsg)
	e.deps.Events.CloseRun(req.RunID)

	// ③ 摘要回写主库（**不同事务、最终一致**；失败只影响列表新鲜度）
	if e.deps.Summary != nil {
		_ = e.deps.Summary.AddTokensAndStatus(ctx, req.SessionID, tokens, final)
	}

	if final == model.RunFailed {
		return fmt.Errorf("agent: Run 失败: %s %s", errCode, errMsg)
	}
	return nil
}

// loop 主循环：组装 → 流式 → 落消息 → 工具 → 回灌，直到终态或步数上限。
func (e *Engine) loop(ctx context.Context, sdb SessionDB, req RunRequest) (
	final model.RunStatus, stopReason, errCode, errMsg string, tokens int64,
) {
	// P1：per-Run 用量累计，返回前统一回写 runs 表（取消路径也记账——
	// WithoutCancel 保证写入不被请求取消吞掉）。
	var runPrompt, runCompletion int64
	defer func() {
		_ = sdb.AddRunUsage(context.WithoutCancel(ctx), req.RunID, runPrompt, runCompletion)
	}()

	history, err := e.loadHistory(ctx, sdb)
	if err != nil {
		return model.RunFailed, "", "history_load_failed", err.Error(), 0
	}

	userInput := req.UserInput
	for step := 0; step < e.deps.MaxSteps; step++ {
		if ctx.Err() != nil {
			return model.RunCancelled, "context_canceled", "", "", tokens
		}
		// 每步续期会话租约（C-6）：长 Run 不因租约 TTL 到期而被误接管。
		e.renewLease(ctx, req)

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
		runPrompt += out.promptTokens
		runCompletion += out.completionTokens
		if status != "" { // 非空表示本轮异常终止
			return status, "", code, failMsg, tokens
		}

		// 落 assistant 消息（里程碑事件，delta 不落库）
		assistantMsg := &model.Message{
			ID:        newID(),
			RunID:     req.RunID,
			Role:      model.RoleAssistant,
			Content:   out.content,
			CreatedAt: e.deps.Now(),
		}
		if len(out.toolCalls) > 0 {
			if raw, mErr := json.Marshal(out.toolCalls); mErr == nil {
				assistantMsg.ToolCallsJSON = raw
			}
		}
		if err := sdb.AppendMessage(ctx, assistantMsg); err != nil {
			return model.RunFailed, "", "persist_failed", err.Error(), tokens
		}
		if e.deps.Summary != nil {
			_ = e.deps.Summary.TouchMessage(ctx, req.SessionID, e.deps.Now())
		}
		e.publish(req.RunID, events.TypeMessageEnd, map[string]any{
			"message_id": assistantMsg.ID,
			"content":    out.content,
			"seq":        assistantMsg.Seq,
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
			// tool 消息同样落库（消息最终内容以 DB 为准）
			if err := sdb.AppendMessage(ctx, toolMsg); err != nil {
				return model.RunFailed, "", "persist_failed", err.Error(), tokens
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

// streamOutcome 单轮流式结果。
type streamOutcome struct {
	content   string
	toolCalls []llm.ToolCall
	// promptTokens / completionTokens 本轮流式用量（P1：随终态回写 runs 表，
	// OpenAI 兼容端点与 UI 的 usage 以此为准）。
	promptTokens     int64
	completionTokens int64
}

// streamOnce 执行一轮模型调用并消费流。
// 返回的 status 非空表示异常终态（cancelled/failed）。
func (e *Engine) streamOnce(ctx context.Context, req RunRequest, provider llm.Provider, res prompt.BuildResult) (
	out streamOutcome, status model.RunStatus, errCode, errMsg string, tokens int64,
) {
	stream, err := provider.ChatStream(ctx, res.Request)
	if err != nil {
		if ctx.Err() != nil {
			return out, model.RunCancelled, "context_canceled", "", 0
		}
		le := llm.AsError(err)
		return out, model.RunFailed, string(le.Kind), le.Message, 0
	}
	defer func() { _ = stream.Close() }()

	var content strings.Builder
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
				return out, model.RunCancelled, "context_canceled", "", tokens
			}
			le := llm.AsError(rErr)
			return out, model.RunFailed, string(le.Kind), le.Message, tokens
		}
		if ch.Delta != "" {
			content.WriteString(ch.Delta)
			// delta 只发事件，不落库（ADR-003）
			e.publish(req.RunID, events.TypeMessageDelta, map[string]any{
				"message_id": msgID,
				"delta":      ch.Delta,
			})
		}
		if ch.Usage != nil {
			tokens += int64(ch.Usage.TotalTokens)
			out.promptTokens += int64(ch.Usage.PromptTokens)
			out.completionTokens += int64(ch.Usage.CompletionTokens)
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
	return out, "", "", "", tokens
}

// execTool 执行单次工具调用（含审批）。返回的 cancelled 表示被中断。
func (e *Engine) execTool(ctx context.Context, sdb SessionDB, req RunRequest, tc llm.ToolCall) (
	msg *model.Message, cancelled bool,
) {
	eventID := newID()
	// 风险级由工具契约提供（Catalog 未注入时按 write 保守处理）
	risk := model.RiskWrite
	if e.deps.Catalog != nil {
		if r, ok := e.deps.Catalog.Risk(tc.Name); ok {
			risk = r
		}
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
	if err := sdb.InsertToolEvent(ctx, ev); err != nil {
		return toolMessage(tc, "工具事件落库失败: "+err.Error()), false
	}
	e.publish(req.RunID, events.TypeToolStarted, map[string]any{
		"tool_event_id": eventID,
		"tool_name":     tc.Name,
		"risk":          string(risk),
	})

	// 审批
	rules, _ := sdb.ApprovalRules(ctx)
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
		AccessMode:        req.AccessMode,
		Rules:             rules,
		RequestedByDevice: requester,
		RequiredDeviceID:  required,
	})

	decision := verdict.Auto
	reason := verdict.Reason
	if verdict.NeedApproval {
		if _, err := e.deps.Gate.RegisterWithDevice(eventID, requester, required); err != nil {
			_ = sdb.FinishToolEvent(ctx, eventID, model.ToolError, "", "", err.Error(), localExec)
			return toolMessage(tc, "审批登记失败: "+err.Error()), false
		}
		e.publish(req.RunID, events.TypeApprovalRequest, map[string]any{
			"approval_id":   eventID,
			"tool_event_id": eventID,
			"tool_name":     tc.Name,
			"risk":          string(risk),
			"reason":        reason,
			"session_id":    req.SessionID,
			// 新增字段（协议向后兼容，仅新增不改形状）：前端据此显示「需在端侧确认」
			"requested_by_device": requester,
			"required_device_id":  required,
		})
		d, r := e.deps.Gate.Await(ctx, eventID)
		decision = d
		if r != "" {
			reason = r
		}
		e.publish(req.RunID, events.TypeApprovalDecision, map[string]any{
			"approval_id":         eventID,
			"decision":            string(decision),
			"reason":              reason,
			"requested_by_device": requester,
			"required_device_id":  required,
		})
		if ctx.Err() != nil {
			_ = sdb.FinishToolEvent(ctx, eventID, model.ToolDenied, "", "", "Run 已取消", localExec)
			return toolMessage(tc, "denied: Run 已取消"), true
		}
	}

	if decision == approval.Deny {
		// 拒绝原因回传模型，让它换方案继续（不中断 Run）
		_ = sdb.FinishToolEvent(ctx, eventID, model.ToolDenied, "", "", reason, localExec)
		e.publish(req.RunID, events.TypeToolResult, map[string]any{
			"tool_event_id": eventID,
			"status":        string(model.ToolDenied),
			"error":         reason,
		})
		return toolMessage(tc, "denied: "+reason), false
	}
	if decision == approval.AllowSession {
		_ = sdb.AddApprovalRule(ctx, tc.Name, "")
	}

	// 执行
	if e.deps.Tools == nil {
		const msg = "工具执行器未注册（A 阶段仅协议骨架）"
		_ = sdb.FinishToolEvent(ctx, eventID, model.ToolError, "", "", msg, localExec)
		e.publish(req.RunID, events.TypeToolResult, map[string]any{
			"tool_event_id": eventID,
			"status":        string(model.ToolError),
			"error":         msg,
		})
		return toolMessage(tc, msg), false
	}

	toolCtx, cancel := context.WithTimeout(ctx, e.deps.ToolTimeout)
	defer cancel()
	result, runErr := e.deps.Tools.Run(toolCtx, ToolRunRequest{
		RunID:       req.RunID,
		SessionID:   req.SessionID,
		ToolEventID: eventID,
		ToolName:    tc.Name,
		ArgsJSON:    string(tc.Arguments),
		AccessMode:  string(req.AccessMode),
	})
	if runErr != nil {
		errText := runErr.Error()
		_ = sdb.FinishToolEvent(ctx, eventID, model.ToolError, "", "", errText, localExec)
		e.publish(req.RunID, events.TypeToolResult, map[string]any{
			"tool_event_id": eventID,
			"status":        string(model.ToolError),
			"error":         errText,
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
	_ = sdb.FinishToolEvent(ctx, eventID, result.Status, result.ResultJSON, result.ResultBlob,
		result.Error, execInfo)
	e.publish(req.RunID, events.TypeToolResult, map[string]any{
		"tool_event_id": eventID,
		"status":        string(result.Status),
		"result":        result.ResultJSON,
		"error":         result.Error,
		"session_id":    req.SessionID,
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
	return out, nil
}

func (e *Engine) transition(ctx context.Context, sdb SessionDB, runID string, to model.RunStatus, stopReason, errCode, errMsg string) error {
	return sdb.TransitionRun(ctx, runID, to, stopReason, errCode, errMsg)
}

func (e *Engine) publish(runID string, typ events.Type, payload any) {
	// 事件发布失败不应影响 Run（预览通道，DB 才是权威）
	_, _ = e.deps.Events.Publish(runID, typ, payload)
}

func (e *Engine) publishStatus(runID, sessionID string, status model.RunStatus, stopReason, errCode, errMsg string) {
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
