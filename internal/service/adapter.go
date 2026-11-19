// Package service 是用例编排层：把 store（适配器）与 agent（领域）缝在一起。
//
// 分层约束（docs/架构设计文档.md §2.2）：
//   - api → service → agent/store，api 不得直连 store
//   - agent 只依赖自己定义的窄接口，本包的 adapter 负责把 store 适配过去（依赖倒置）
package service

import (
	"context"
	"encoding/json"

	"harness-go/internal/agent"
	"harness-go/internal/approval"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// NewSessionProvider 把 store.SessionStore 适配成 agent.SessionProvider。
func NewSessionProvider(ss *store.SessionStore) agent.SessionProvider {
	return &sessionProvider{ss: ss}
}

type sessionProvider struct{ ss *store.SessionStore }

func (p *sessionProvider) Session(ctx context.Context, sessionID string) (agent.SessionDB, error) {
	sdb, err := p.ss.Acquire(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return &sessionDBAdapter{sdb: sdb}, nil
}

type sessionDBAdapter struct{ sdb *store.SessionDB }

func (a *sessionDBAdapter) AppendMessage(ctx context.Context, m *model.Message) error {
	return a.sdb.Messages().Append(ctx, m)
}

// ListMessagesTail 取最近 limit 条**未置灰**消息（seq 升序）。
//
// 走存储层新增的 ListVisibleTail（`ORDER BY seq DESC LIMIT ?` 后反转），
// 而不是复用 ListVisible：后者受 `limit>500→100` 防洪闸约束且取头不取尾，
// 引擎传 1000 会被静默截成最旧 100 条（详见 store.MessageRepo.ListTail 注释）。
func (a *sessionDBAdapter) ListMessagesTail(ctx context.Context, limit int) ([]model.Message, error) {
	return a.sdb.Messages().ListVisibleTail(ctx, limit)
}

func (a *sessionDBAdapter) GetRun(ctx context.Context, id string) (*model.Run, error) {
	return a.sdb.Runs().Get(ctx, id)
}

func (a *sessionDBAdapter) TransitionRun(ctx context.Context, id string, to model.RunStatus, stopReason, errCode, errMsg string) error {
	return a.sdb.Runs().Transition(ctx, id, to, stopReason, errCode, errMsg)
}

func (a *sessionDBAdapter) InsertToolEvent(ctx context.Context, ev *model.ToolEvent) error {
	return a.sdb.ToolEvents().Insert(ctx, ev)
}

func (a *sessionDBAdapter) FinishToolEvent(ctx context.Context, id string, status model.ToolEventStatus, resultJSON, resultBlob, errMsg string, exec agent.ToolEventExec) error {
	var raw json.RawMessage
	if resultJSON != "" {
		raw = json.RawMessage(resultJSON)
	}
	return a.sdb.ToolEvents().Finish(ctx, id, status, raw, resultBlob, errMsg, exec)
}

func (a *sessionDBAdapter) ApprovalRules(ctx context.Context) ([]approval.Rule, error) {
	rules, err := a.sdb.ApprovalRules().List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]approval.Rule, 0, len(rules))
	for _, r := range rules {
		out = append(out, approval.Rule{ToolName: r.ToolName, Pattern: r.Pattern})
	}
	return out, nil
}

func (a *sessionDBAdapter) AddApprovalRule(ctx context.Context, toolName, pattern string) error {
	return a.sdb.ApprovalRules().Add(ctx, toolName, pattern)
}

// AddRunUsage 回写单次 Run 的 token 用量（P1）。
func (a *sessionDBAdapter) AddRunUsage(ctx context.Context, runID string, promptTokens, completionTokens, cachedTokens int64, isEstimated bool) error {
	return a.sdb.Runs().AddUsage(ctx, runID, promptTokens, completionTokens, cachedTokens, isEstimated)
}

// NewSummaryWriter 把 store.MainStore 适配成 agent.SummaryWriter。
func NewSummaryWriter(m *store.MainStore) agent.SummaryWriter {
	return &summaryWriter{m: m}
}

// SessionChangePublisher 会话摘要变更的观察者（consumer-side 小接口）。
//
// 用途：把「主库会话摘要被改写了」这件事暴露给上层（cmd 装配时接 sessionbus），
// 从而让各端的对话列表能实时刷新「运行中 / token 用量 / 消息时间」。
//
// 为什么定义在 service 而不是直接依赖 sessionbus：
//   - service 是最底层用例层，反向 import 一个传输侧广播包会污染依赖方向；
//   - 多数单测不需要总线，nil 时必须能正常工作（本包所有构造点默认就是 nil）。
type SessionChangePublisher interface {
	// PublishSessionUpdated 会话摘要已变化（高频，允许实现方节流合并）。
	PublishSessionUpdated(sessionID string)
	// PublishSessionStatusChanged Run 状态跃迁（低频，要求低延迟）。
	PublishSessionStatusChanged(sessionID string)
}

// NewSummaryWriterWithChanges 构造带变更观察者的 summaryWriter。
//
// 既有调用点（测试与 main 的历史路径）继续用 NewSummaryWriter 即可，行为完全不变；
// 需要实时刷新对话列表的装配点（cmd）改用本函数注入总线。
func NewSummaryWriterWithChanges(m *store.MainStore, pub SessionChangePublisher) agent.SummaryWriter {
	return &summaryWriter{m: m, changes: pub}
}

type summaryWriter struct {
	m *store.MainStore
	// changes 会话摘要变更观察者（可为 nil：nil 时退化为纯回写，行为与旧版一致）。
	changes SessionChangePublisher
}

func (w *summaryWriter) TouchMessage(ctx context.Context, sessionID string, at int64) error {
	if err := w.m.Sessions().TouchMessage(ctx, sessionID, at); err != nil {
		return err
	}
	// 必须在**写库成功之后**通知：先通知会让客户端回查读到旧值。
	if w.changes != nil {
		w.changes.PublishSessionUpdated(sessionID)
	}
	return nil
}

func (w *summaryWriter) AddTokensAndStatus(ctx context.Context, sessionID string, tokens int64, status model.RunStatus) error {
	if err := w.m.Sessions().AddTokensAndStatus(ctx, sessionID, tokens, status); err != nil {
		return err
	}
	if w.changes == nil {
		return nil
	}
	// token 累加与消息时间戳在**每条消息、每个工具结果**落库时都会触发，
	// 属高频变更 → 交给实现方节流合并（sessionbus 默认 1s 窗口）。
	w.changes.PublishSessionUpdated(sessionID)
	// Run 状态跃迁单独走低延迟通道。**终态同样要立即推**：
	// 「运行中」徽标的出现（pending/running）与消失（completed/failed/cancelled）
	// 都是用户盯着看的变化，塞进 1 秒合并窗口会让徽标滞后，观感上像卡住了。
	// 本方法在每次 Run 生命周期内只被调用 2~3 次，直接推不会造成刷屏。
	if status != "" {
		w.changes.PublishSessionStatusChanged(sessionID)
	}
	return nil
}
