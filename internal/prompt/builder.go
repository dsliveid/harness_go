// Package prompt 负责上下文组装与 token 预算。
//
// 命名说明：不叫 context，避免与标准库冲突（docs/架构设计文档.md §4.2）。
//
// 组装顺序（前缀稳定，保护 provider 前缀缓存）：
//
//	[基础系统提示] → [SOP 全量] → [项目摘要] → [deferred 工具索引]
//	→ [历史消息] → [动态段 + 本轮输入]（动态内容必须置尾）
//
// 工具分层（docs/插件体系设计.md §3.3）：
//   - core      → 全量定义进 tools 数组
//   - deferred  → 只把「名称 + 一行描述」放进 system 索引段
//   - on_demand → 完全不注入（模型不可自主发现）
package prompt

import (
	"encoding/json"
	"fmt"
	"strings"

	"harness-go/internal/llm"
	"harness-go/internal/tokenizer"
)

// Tier 工具注入档位。
type Tier string

const (
	// TierCore 基础工具：全量定义注入
	TierCore Tier = "core"
	// TierDeferred 按需工具：仅名称 + 一行描述进索引
	TierDeferred Tier = "deferred"
	// TierOnDemand 手动工具：零注入
	TierOnDemand Tier = "on_demand"
)

// ToolRef 组装期使用的工具引用（与 pkg/toolapi 解耦，A-10 负责适配）。
type ToolRef struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Tier        Tier
}

// Input 一次上下文组装的输入。
type Input struct {
	Model          string
	SystemBase     string        // 基础系统提示（角色、规则）
	SOP            []string      // SOP 全量注入（建议 ≤6 条，超限由上层拒绝写入）
	ProjectSummary string        // 项目约束摘要（3 行内）
	History        []llm.Message // 历史消息（不含本轮输入）
	Tools          []ToolRef     // 全部候选工具（由调用方按白名单/风险过滤）
	// ActiveTools 本轮经动态检索命中、需要「注入全量定义」的 deferred 工具名（D-2）。
	// 铁律：这只是「注入深度」的提升，不改变工具能否被调用 ——
	// 未列出的 deferred 工具仍可执行（宽进严出），只是模型看不到其 schema。
	ActiveTools []string
	DynamicTail string // 动态内容（检索结果/运行时锚注），**必须置尾**
	UserInput   string // 本轮用户输入
}

// Breakdown token 四段明细（UI 与日志都要可见）。
type Breakdown struct {
	System  int `json:"system"`
	History int `json:"history"`
	Tools   int `json:"tools"`
	Input   int `json:"input"`
	Total   int `json:"total"`
}

// BuildResult 组装结果。
type BuildResult struct {
	Request         llm.ChatRequest
	Breakdown       Breakdown
	InjectedCore    []string // 全量注入的工具名（core 档 + 检索命中的 deferred）
	IndexedDeferred []string // 只进索引的工具名
	SkippedOnDemand []string // 完全未注入的工具名
	// PromotedTools 本轮由动态检索「提升为全量注入」的 deferred 工具名（D-2）。
	// 与 InjectedCore 的差集即为命中项，便于 /tools/injection 展示命中来源。
	PromotedTools  []string
	Estimated      bool // 计数是否为估算（UI 需标注 ≈）
	OverBudget     bool // 超出总预算（上层决定压缩或明确报错，禁止静默截断）
	DroppedHistory int  // 因预算裁剪掉的历史消息数
}

// Limits 预算与注入限制。
type Limits struct {
	MaxTotal         int // 总 token 预算，0 = 不限
	MaxHistory       int // 历史段上限，0 = 不限
	MaxDeferredIndex int // deferred 索引条数上限，默认 60
	MaxSOP           int // SOP 条数上限（硬约束在写入侧，这里做二次校验）
}

// DefaultLimits 返回默认限制（软上限，仅告警不阻断）。
func DefaultLimits() Limits {
	return Limits{MaxDeferredIndex: 60, MaxSOP: 6}
}

// Builder 上下文组装器。
type Builder struct {
	counter tokenizer.Counter
	limits  Limits
}

// NewBuilder 创建组装器。
func NewBuilder(counter tokenizer.Counter, limits Limits) *Builder {
	if counter == nil {
		counter = tokenizer.NewHeuristic()
	}
	if limits.MaxDeferredIndex <= 0 {
		limits.MaxDeferredIndex = 60
	}
	if limits.MaxSOP <= 0 {
		limits.MaxSOP = 6
	}
	return &Builder{counter: counter, limits: limits}
}

// CounterName 返回当前计数器名（可观测性）。
func (b *Builder) CounterName() string { return b.counter.Name() }

// Build 组装请求。
func (b *Builder) Build(in Input) BuildResult {
	core, deferred, onDemand := splitTools(in.Tools)

	// D-2：把动态检索命中的 deferred 工具提升为「全量定义注入」。
	// 这是 Token 治理的主收益点：deferred 档平时只占 ~18 token/条的索引成本，
	// 命中后才付出全量 schema 成本，避免 50 个工具每轮都全量注入（≈16k token）。
	//
	// 铁律（宽进严出）：未命中的 deferred 工具**仍然可执行**，只是模型看不到其 schema；
	// 本函数只影响「注入深度」，绝不影响「能否调用」。
	deferred, promoted := promoteActive(deferred, in.ActiveTools)

	// deferred 索引截断（超出上限只保留前 N 条，并计入结果便于告警）
	if len(deferred) > b.limits.MaxDeferredIndex {
		deferred = deferred[:b.limits.MaxDeferredIndex]
	}
	// SOP 上限二次校验（防调用方绕过写入侧校验）
	sop := in.SOP
	if len(sop) > b.limits.MaxSOP {
		sop = sop[:b.limits.MaxSOP]
	}

	system := b.buildSystem(in, sop, deferred)

	history := in.History
	dropped := 0
	if b.limits.MaxHistory > 0 {
		history, dropped = b.trimHistory(history, b.limits.MaxHistory)
	}

	// 动态段必须置尾：拼到本轮 user 消息正文前，绝不进 system（否则前缀缓存全废）
	userContent := in.UserInput
	if in.DynamicTail != "" {
		userContent = in.DynamicTail + "\n\n" + in.UserInput
	}

	msgs := make([]llm.Message, 0, len(history)+1)
	msgs = append(msgs, history...)
	// 本轮无用户输入（如工具回灌后的续轮）时不追加空 user 消息
	if userContent != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: userContent})
	}

	toolSpecs := make([]llm.ToolSpec, 0, len(core)+len(promoted))
	coreNames := make([]string, 0, len(core)+len(promoted))
	for _, t := range core {
		toolSpecs = append(toolSpecs, llm.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
		coreNames = append(coreNames, t.Name)
	}
	// 命中提升的工具与 core 一样注入全量定义（模型才能正确构造参数）。
	for _, t := range promoted {
		toolSpecs = append(toolSpecs, llm.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
		coreNames = append(coreNames, t.Name)
	}
	deferredNames := make([]string, 0, len(deferred))
	for _, t := range deferred {
		deferredNames = append(deferredNames, t.Name)
	}
	onDemandNames := make([]string, 0, len(onDemand))
	for _, t := range onDemand {
		onDemandNames = append(onDemandNames, t.Name)
	}

	bd := b.computeBreakdown(system, history, toolSpecs, userContent)
	overBudget := b.limits.MaxTotal > 0 && bd.Total > b.limits.MaxTotal

	return BuildResult{
		Request: llm.ChatRequest{
			Model:    in.Model,
			System:   system,
			Messages: msgs,
			Tools:    toolSpecs,
			Stream:   true,
		},
		Breakdown:       bd,
		InjectedCore:    coreNames,
		IndexedDeferred: deferredNames,
		SkippedOnDemand: onDemandNames,
		PromotedTools:   promotedNames(promoted),
		Estimated:       b.counter.Estimated(),
		OverBudget:      overBudget,
		DroppedHistory:  dropped,
	}
}

// buildSystem 构建系统提示（顺序固定，前缀稳定）。
func (b *Builder) buildSystem(in Input, sop []string, deferred []ToolRef) string {
	var sb strings.Builder
	sb.WriteString(strings.TrimSpace(in.SystemBase))

	if len(sop) > 0 {
		sb.WriteString("\n\n## 项目规范（SOP）\n")
		for i, s := range sop {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, strings.TrimSpace(s)))
		}
	}
	if in.ProjectSummary != "" {
		sb.WriteString("\n## 项目约束\n")
		sb.WriteString(strings.TrimSpace(in.ProjectSummary))
		sb.WriteString("\n")
	}
	if len(deferred) > 0 {
		// 只放名称 + 一行描述：约 18 token/条，模型可据此判断是否需要按需加载
		sb.WriteString("\n## 可按需调用的工具（需要时请显式加载）\n")
		for _, t := range deferred {
			sb.WriteString(fmt.Sprintf("- %s — %s\n", t.Name, firstLine(t.Description)))
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// trimHistory 从最旧开始丢弃历史，直到满足预算（保留最近消息）。
func (b *Builder) trimHistory(history []llm.Message, maxTokens int) ([]llm.Message, int) {
	if len(history) == 0 {
		return history, 0
	}
	msgs := toCountMessages(history)
	if b.counter.CountMessages(msgs) <= maxTokens {
		return history, 0
	}
	// 从头逐步丢弃（每轮丢弃 1/4，避免 O(n²) 估算）
	keep := len(history)
	for keep > 0 {
		keep = keep - maxInt(1, len(history)/4)
		if keep < 0 {
			keep = 0
		}
		candidate := history[len(history)-keep:]
		if b.counter.CountMessages(toCountMessages(candidate)) <= maxTokens {
			return candidate, len(history) - keep
		}
	}
	return nil, len(history)
}

func (b *Builder) computeBreakdown(system string, history []llm.Message, tools []llm.ToolSpec, userContent string) Breakdown {
	sysTokens := b.counter.CountText(system)
	histTokens := b.counter.CountMessages(toCountMessages(history))
	toolTokens := b.counter.CountTools(toCountToolDefs(tools))
	inputTokens := b.counter.CountMessages([]tokenizer.Message{{Role: "user", Content: userContent}})
	return Breakdown{
		System:  sysTokens,
		History: histTokens,
		Tools:   toolTokens,
		Input:   inputTokens,
		Total:   sysTokens + histTokens + toolTokens + inputTokens,
	}
}

// promoteActive 把动态检索命中的 deferred 工具从「索引档」提升为「全量注入档」。
//
// 返回 (剩余仍只进索引的 deferred, 被提升的工具)。
// 匹配用工具名精确相等（大小写敏感，与 registry 的键一致）。
// ActiveTools 中的未知名字被静默忽略 —— 检索结果可能来自过期索引，
// 不能因为索引里有而注册表里没有就报错（宽进严出的另一半：宁可少注入不可失败）。
func promoteActive(deferred []ToolRef, active []string) (rest, promoted []ToolRef) {
	if len(deferred) == 0 || len(active) == 0 {
		return deferred, nil
	}
	want := make(map[string]struct{}, len(active))
	for _, n := range active {
		if n = strings.TrimSpace(n); n != "" {
			want[n] = struct{}{}
		}
	}
	if len(want) == 0 {
		return deferred, nil
	}
	rest = make([]ToolRef, 0, len(deferred))
	for _, t := range deferred {
		if _, ok := want[t.Name]; ok {
			promoted = append(promoted, t)
			continue
		}
		rest = append(rest, t)
	}
	return rest, promoted
}

// promotedNames 提取提升工具名（保序）。
func promotedNames(tools []ToolRef) []string {
	if len(tools) == 0 {
		return nil
	}
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

func splitTools(tools []ToolRef) (core, deferred, onDemand []ToolRef) {
	for _, t := range tools {
		switch t.Tier {
		case TierCore:
			core = append(core, t)
		case TierOnDemand:
			onDemand = append(onDemand, t)
		default: // 空值与其他值一律按 deferred（保守：只进索引，不占全量成本）
			deferred = append(deferred, t)
		}
	}
	return core, deferred, onDemand
}

func toCountMessages(msgs []llm.Message) []tokenizer.Message {
	out := make([]tokenizer.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, tokenizer.Message{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			HasTools:   len(m.ToolCalls) > 0,
		})
	}
	return out
}

func toCountToolDefs(tools []llm.ToolSpec) []tokenizer.ToolDef {
	out := make([]tokenizer.ToolDef, 0, len(tools))
	for _, t := range tools {
		out = append(out, tokenizer.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			SchemaJSON:  string(t.InputSchema),
		})
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	// 索引条目要短：单条描述截断到 60 字，避免索引本身吃掉预算
	if r := []rune(s); len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
