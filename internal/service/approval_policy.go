package service

import (
	"context"
	"log/slog"
	"strings"

	"harness-go/internal/approval"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// ApprovalPolicy 审批规则的**唯一合并入口**（阶段四：项目级规则下发引擎）。
//
// 为什么需要一个独立 resolver：
//   - 引擎（internal/agent）只认识「一份 approval.Rule 列表」，不应该感知「项目」概念，
//     否则 agent 包会被迫依赖 store/model.Project，破坏端口隔离；
//   - 规则来源有两个（会话级「本会话允许」+ 项目级声明规则），合并/去重/降级策略
//     必须只有一份实现，否则就是第二条判定路径 —— 安全语义迟早漂移。
//
// 合并顺序与语义：
//  1. 会话库规则（用户在工作流中点「本会话允许」产生的）—— 最贴近当前对话，优先保留；
//  2. 会话所属项目的声明式规则 —— 追加在会话规则之后，按 (toolName, pattern) 去重。
//
// 失败策略（关键）：任一侧读取失败**只降级不中断**。规则读不到的结果是「该放行的仍需审批」，
// 属于安全侧的保守失败；绝不能因为规则表抖动而让整个 Run 失败。
type ApprovalPolicy struct {
	main     *store.MainStore
	sessions *store.SessionStore
	log      *slog.Logger
}

// NewApprovalPolicy 创建规则合并器。
//
// sessions 用于按需取会话库句柄（懒建：纯对话/无项目的会话不会因此多开库）。
func NewApprovalPolicy(main *store.MainStore, sessions *store.SessionStore) *ApprovalPolicy {
	return &ApprovalPolicy{main: main, sessions: sessions, log: slog.Default()}
}

// RulesFor 返回某会话生效的完整审批规则集合（会话级 + 项目级去重合并）。
//
// 实现要点：
//   - 先取会话归属（一次主库读）；project_id 为空 → 直接返回会话规则，纯对话零额外成本；
//   - 会话规则读失败 → 记 warning 并继续尝试项目规则（不 return，尽量多放行声明式规则）；
//   - 项目规则读失败 → 记 warning 并返回已取得的会话规则。
func (p *ApprovalPolicy) RulesFor(ctx context.Context, sessionID string) ([]approval.Rule, error) {
	if p == nil || p.main == nil {
		return nil, nil
	}
	sessionRules := p.sessionRules(ctx, sessionID)

	projectID := p.projectIDOf(ctx, sessionID)
	if projectID == "" {
		return sessionRules, nil
	}
	projectRules, err := p.main.Projects().ListRules(ctx, projectID)
	if err != nil {
		// 降级：项目规则不可用时不放大放行面，仅用会话规则。
		p.log.Warn("审批策略：读取项目规则失败，已降级为仅会话规则",
			"session_id", sessionID, "project_id", projectID, "err", err)
		return sessionRules, nil
	}
	return mergeRules(sessionRules, projectRules), nil
}

// sessionRules 读取会话库审批规则（失败只记日志、返回 nil）。
func (p *ApprovalPolicy) sessionRules(ctx context.Context, sessionID string) []approval.Rule {
	if p.sessions == nil || sessionID == "" {
		return nil
	}
	sdb, err := p.sessions.Acquire(ctx, sessionID)
	if err != nil {
		p.log.Warn("审批策略：获取会话库失败，本次无会话规则",
			"session_id", sessionID, "err", err)
		return nil
	}
	rows, err := sdb.ApprovalRules().List(ctx)
	if err != nil {
		p.log.Warn("审批策略：读取会话规则失败，本次无会话规则",
			"session_id", sessionID, "err", err)
		return nil
	}
	out := make([]approval.Rule, 0, len(rows))
	for _, r := range rows {
		out = append(out, approval.Rule{ToolName: r.ToolName, Pattern: r.Pattern})
	}
	return out
}

// projectIDOf 读会话归属项目（任何失败一律视作「无项目」，走纯对话路径）。
func (p *ApprovalPolicy) projectIDOf(ctx context.Context, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	sess, err := p.main.Sessions().Get(ctx, sessionID)
	if err != nil || sess == nil {
		return ""
	}
	return strings.TrimSpace(sess.ProjectID)
}

// mergeRules 合并会话规则与项目规则，按 (ToolName, Pattern) 去重。
//
// 去重键是对齐 Gate 的匹配语义：(ToolName 相同 && (Pattern 为空 || 参数前缀命中))。
// 结构完全相同（同工具同 pattern）的规则在 Gate 里产生的效果一致，保留一份即可。
// 用 map 而非 O(n²) 遍历：规则总量小，但幂等与可读性更重要。
func mergeRules(sessionRules []approval.Rule, projectRules []model.ProjectApprovalRule) []approval.Rule {
	seen := make(map[ruleKey]struct{}, len(sessionRules)+len(projectRules))
	out := make([]approval.Rule, 0, len(sessionRules)+len(projectRules))
	push := func(toolName, pattern string) {
		if toolName = strings.TrimSpace(toolName); toolName == "" {
			return // 脏数据兜底：空工具名规则永不命中，不进入下发集合
		}
		k := ruleKey{tool: toolName, pattern: strings.TrimSpace(pattern)}
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		out = append(out, approval.Rule{ToolName: k.tool, Pattern: k.pattern})
	}
	for _, r := range sessionRules {
		push(r.ToolName, r.Pattern)
	}
	for _, r := range projectRules {
		push(r.ToolName, r.Pattern)
	}
	return out
}

// ruleKey 规则去重键。
type ruleKey struct {
	tool    string
	pattern string
}
