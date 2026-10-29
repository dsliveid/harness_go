// Package approval 实现审批闸门：风险级 → 会话规则 → 访问模式。
//
// 核心语义（docs/架构设计文档.md §5.3）：
//   - readonly 工具默认自动放行
//   - 访问模式 confirm：write/command/danger 都需人工审批
//   - 访问模式 full：write/command 自动放行，**danger 仍需逐次审批**（不可永久放行）
//   - 会话级「本会话允许」规则命中即自动放行（仅作用于该会话）
//   - 等待超过 Timeout（默认 5min）**视为拒绝**，拒绝原因回传模型继续对话
package approval

import (
	"context"
	"fmt"
	"sync"
	"time"

	"harness-go/internal/model"
)

// Decision 审批结果。
type Decision string

const (
	// AllowOnce 仅本次允许
	AllowOnce Decision = "allow_once"
	// AllowSession 本会话允许（后续同类调用免审）
	AllowSession Decision = "allow_session"
	// Deny 拒绝（原因回传模型）
	Deny Decision = "deny"
)

// AccessMode 会话访问模式。
type AccessMode string

const (
	// ModeConfirm 每次确认（默认）
	ModeConfirm AccessMode = "confirm"
	// ModeFull 完全放行（danger 除外）
	ModeFull AccessMode = "full"
)

// Rule 会话级审批规则（由 AllowSession 决策产生）。
type Rule struct {
	ToolName string
	Pattern  string // 命令前缀 / 路径前缀；空表示匹配该工具全部调用
}

// Request 审批请求。
type Request struct {
	ApprovalID  string
	RunID       string
	ToolEventID string
	ToolName    string
	Risk        model.Risk
	ArgsJSON    string
	AccessMode  AccessMode
	Rules       []Rule
}

// Verdict 判定结论。
type Verdict struct {
	NeedApproval bool // true = 必须人工审批
	Auto         Decision
	Reason       string
}

// DefaultTimeout 审批等待默认超时（超时视为拒绝）。
const DefaultTimeout = 5 * time.Minute

// Gate 审批闸门（并发安全）。
//
// 职责分工：Evaluate 做同步判定；Register/Resolve/Await 处理人工审批的异步等待。
type Gate struct {
	timeout time.Duration

	mu      sync.Mutex
	pending map[string]*waiter
}

type waiter struct {
	ch     chan Decision
	reason string
}

// NewGate 创建闸门；timeout<=0 用 5min。
func NewGate(timeout time.Duration) *Gate {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Gate{timeout: timeout, pending: make(map[string]*waiter)}
}

// Timeout 返回等待超时。
func (g *Gate) Timeout() time.Duration { return g.timeout }

// Evaluate 同步判定是否需要人工审批。
//
// 判定顺序（短路）：readonly 放行 → 会话规则命中放行 → 访问模式 → 需审批。
func (g *Gate) Evaluate(req Request) Verdict {
	// ① 只读工具永不打扰用户
	if req.Risk == model.RiskReadonly {
		return Verdict{Auto: AllowOnce, Reason: "只读工具自动放行"}
	}

	// ② 会话级规则命中（「本会话允许」的落地）
	for _, r := range req.Rules {
		if r.ToolName != req.ToolName {
			continue
		}
		if r.Pattern == "" || hasPrefixString(req.ArgsJSON, r.Pattern) {
			return Verdict{Auto: AllowOnce, Reason: "命中本会话允许规则"}
		}
	}

	// ③ 访问模式
	switch req.AccessMode {
	case ModeFull:
		if req.Risk == model.RiskDanger {
			return Verdict{NeedApproval: true, Reason: "高危操作必须逐次确认（不可永久放行）"}
		}
		return Verdict{Auto: AllowOnce, Reason: "完全访问模式自动放行"}
	default: // ModeConfirm 或未知模式一律按最严格处理
		return Verdict{NeedApproval: true, Reason: "当前为逐次确认模式"}
	}
}

// Register 登记一个待决审批，返回决策通道。
func (g *Gate) Register(approvalID string) (<-chan Decision, error) {
	if approvalID == "" {
		return nil, fmt.Errorf("approval: approvalID 不能为空")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, exists := g.pending[approvalID]; exists {
		return nil, fmt.Errorf("approval: %s 已在等待中", approvalID)
	}
	w := &waiter{ch: make(chan Decision, 1)}
	g.pending[approvalID] = w
	return w.ch, nil
}

// Resolve 提交决策（由 API 层调用）。返回是否命中待决项。
func (g *Gate) Resolve(approvalID string, d Decision) bool {
	g.mu.Lock()
	w, ok := g.pending[approvalID]
	if ok {
		delete(g.pending, approvalID)
	}
	g.mu.Unlock()
	if !ok {
		return false
	}
	w.ch <- d
	close(w.ch)
	return true
}

// Await 等待决策；**超时或 ctx 取消一律视为 Deny**（原因回传模型）。
func (g *Gate) Await(ctx context.Context, approvalID string) (Decision, string) {
	g.mu.Lock()
	w, ok := g.pending[approvalID]
	g.mu.Unlock()
	if !ok {
		return Deny, "审批请求不存在或已处理"
	}

	timer := time.NewTimer(g.timeout)
	defer timer.Stop()

	select {
	case d, open := <-w.ch:
		if !open {
			return Deny, "审批通道已关闭"
		}
		if d == "" {
			return Deny, "审批结果为空"
		}
		return d, ""
	case <-timer.C:
		g.remove(approvalID)
		return Deny, fmt.Sprintf("审批超时（%s）视为拒绝", g.timeout)
	case <-ctx.Done():
		g.remove(approvalID)
		return Deny, "审批等待被取消"
	}
}

func (g *Gate) remove(approvalID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.pending, approvalID)
}

// PendingCount 待决审批数（监控用）。
func (g *Gate) PendingCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pending)
}

// hasPrefixString 判断原始 JSON 参数里是否包含指定前缀（规则匹配的简化实现）。
//
// 说明：A 阶段用「包含」而非严格解析路径/命令前缀；阶段 F 接入 fs/command 插件后，
// 由插件提供结构化 pattern（路径前缀、命令首词）再收紧。
func hasPrefixString(argsJSON, pattern string) bool {
	if pattern == "" {
		return true
	}
	return len(argsJSON) >= len(pattern) && containsString(argsJSON, pattern)
}

func containsString(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
