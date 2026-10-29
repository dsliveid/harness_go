// Package tokenizer 提供 token 计数：精确（tiktoken/HF，后续阶段接入）与估算（heuristic）。
//
// 设计约束（docs/需求说明文档.md §7.5）：
//   - 计数必须包含「结构开销」：每条消息的 role 包裹、末尾回复引导、tools JSON Schema 全额计入
//   - 估算值必须可识别（Estimated 标志），UI 上要标注「≈」
//   - 已发生用量以 provider 返回的 usage 为准，本包只服务于「未发生预算」
package tokenizer

import (
	"strings"
	"sync"
	"unicode"
)

// Message 计数用的最小消息形态（与 llm.Message 解耦，避免循环依赖）。
type Message struct {
	Role       string
	Content    string
	ToolCallID string
	HasTools   bool // assistant 消息是否携带 tool_calls（额外开销）
}

// ToolDef 工具定义（用于计算 tools 段的上下文成本）。
type ToolDef struct {
	Name        string
	Description string
	SchemaJSON  string
}

// Counter 计数端口。
type Counter interface {
	// Name 实现名（heuristic / tiktoken:o200k_base / ...）
	Name() string
	// Estimated 是否估算（true 表示 UI 需标注 ≈）
	Estimated() bool
	// CountText 纯文本 token 数
	CountText(s string) int
	// CountMessages 消息段（含每条消息的结构开销）
	CountMessages(msgs []Message) int
	// CountTools 工具定义段（name + description + schema 全额计入）
	CountTools(tools []ToolDef) int
}

// 结构开销常量（按 OpenAI chat 模板的实测经验值）。
const (
	perMessageOverhead = 3 // 每条消息 role/分隔符包裹
	replyPriming       = 3 // 末尾引导 assistant 回复
)

// heuristic 字符系数估算：CJK 约 1 token/字，拉丁约 4 字符/token。
type heuristic struct{}

// NewHeuristic 返回字符系数估算计数器。
func NewHeuristic() Counter { return heuristic{} }

// Name 实现 Counter。
func (heuristic) Name() string { return "heuristic" }

// Estimated 实现 Counter。
func (heuristic) Estimated() bool { return true }

// CountText 实现 Counter。
func (heuristic) CountText(s string) int {
	if s == "" {
		return 0
	}
	var cjk, other int
	for _, r := range s {
		if isCJK(r) {
			cjk++
			continue
		}
		other++
	}
	tokens := cjk + (other+3)/4
	if tokens == 0 {
		tokens = 1
	}
	return tokens
}

// CountMessages 实现 Counter。
func (heuristic) CountMessages(msgs []Message) int {
	total := replyPriming
	for _, m := range msgs {
		total += perMessageOverhead
		total += heuristic{}.CountText(m.Role)
		total += heuristic{}.CountText(m.Content)
		if m.ToolCallID != "" {
			total += heuristic{}.CountText(m.ToolCallID)
		}
		if m.HasTools {
			total += perMessageOverhead // tool_calls 结构开销
		}
	}
	return total
}

// CountTools 实现 Counter。
func (heuristic) CountTools(tools []ToolDef) int {
	total := 0
	for _, t := range tools {
		total += heuristic{}.CountText(t.Name)
		total += heuristic{}.CountText(t.Description)
		total += heuristic{}.CountText(t.SchemaJSON)
		total += perMessageOverhead // 每条工具的结构包裹
	}
	return total
}

// isCJK 判定 CJK 字符（中日韩统一表意文字及标点）。
func isCJK(r rune) bool {
	switch {
	case unicode.Is(unicode.Han, r),
		unicode.Is(unicode.Hiragana, r),
		unicode.Is(unicode.Katakana, r),
		unicode.Is(unicode.Hangul, r):
		return true
	}
	return false
}

// ============ 注册表 ============

type prefixRule struct {
	prefix  string
	counter Counter
}

// Registry 按模型名选择计数器。
//
// 匹配顺序：精确名 → 前缀规则（最长优先）→ 回退 heuristic。
// 阶段 D 接入 tiktoken / HF tokenizer 时只需注册进本表，调用方无感。
type Registry struct {
	mu       sync.RWMutex
	exact    map[string]Counter
	prefixes []prefixRule
	fallback Counter
}

// NewRegistry 创建注册表（回退为 heuristic）。
func NewRegistry() *Registry {
	return &Registry{
		exact:    make(map[string]Counter),
		fallback: NewHeuristic(),
	}
}

// Register 注册精确模型名 → 计数器。
func (r *Registry) Register(model string, c Counter) {
	if model == "" || c == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exact[strings.ToLower(model)] = c
}

// RegisterPrefix 注册模型名前缀 → 计数器（如 "gpt-4o" 覆盖 gpt-4o-mini）。
func (r *Registry) RegisterPrefix(prefix string, c Counter) {
	if prefix == "" || c == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prefixes = append(r.prefixes, prefixRule{prefix: strings.ToLower(prefix), counter: c})
	// 最长前缀优先
	for i := 1; i < len(r.prefixes); i++ {
		for j := i; j > 0 && len(r.prefixes[j].prefix) > len(r.prefixes[j-1].prefix); j-- {
			r.prefixes[j], r.prefixes[j-1] = r.prefixes[j-1], r.prefixes[j]
		}
	}
}

// SetFallback 覆盖回退计数器。
func (r *Registry) SetFallback(c Counter) {
	if c == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fallback = c
}

// For 返回适配该模型的计数器（永不返回 nil）。
func (r *Registry) For(model string) Counter {
	key := strings.ToLower(strings.TrimSpace(model))
	r.mu.RLock()
	defer r.mu.RUnlock()
	if c, ok := r.exact[key]; ok {
		return c
	}
	for _, p := range r.prefixes {
		if strings.HasPrefix(key, p.prefix) {
			return p.counter
		}
	}
	return r.fallback
}
