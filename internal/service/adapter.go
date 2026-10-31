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

// ListMessages 只回**未置灰**消息（阶段 G：消息级回滚置灰的消息保留在库，
// 但不进引擎上下文；API 消息列表走 SessionService.Messages，包含置灰消息）。
func (a *sessionDBAdapter) ListMessages(ctx context.Context, afterSeq int64, limit int) ([]model.Message, error) {
	return a.sdb.Messages().ListVisible(ctx, afterSeq, limit)
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

// NewSummaryWriter 把 store.MainStore 适配成 agent.SummaryWriter。
func NewSummaryWriter(m *store.MainStore) agent.SummaryWriter {
	return &summaryWriter{m: m}
}

type summaryWriter struct{ m *store.MainStore }

func (w *summaryWriter) TouchMessage(ctx context.Context, sessionID string, at int64) error {
	return w.m.Sessions().TouchMessage(ctx, sessionID, at)
}

func (w *summaryWriter) AddTokensAndStatus(ctx context.Context, sessionID string, tokens int64, status model.RunStatus) error {
	return w.m.Sessions().AddTokensAndStatus(ctx, sessionID, tokens, status)
}
