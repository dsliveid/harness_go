package service

import (
	"strings"
	"testing"

	"harness-go/internal/llm"
	"harness-go/internal/prompt"
)

// ============ 预算闸门五级降级链路（D-4） ============

// countingBuilder 计数桩：按注入形态返回「可控」的 token 数，
// 让测试能精确控制每一档应用后的计数走向。
//
// 记费模型（heuristic 反推过于脆弱，直接显式建模）：
//   - base：与降级无关的固定开销（system + 输入）；
//   - 每条 MemoryTail 记 20；每条 ActiveTools 提升记 40；
//   - 超出 CoreToolsFloor 的每条 core 工具记 50；每条未折叠 tool 结果记 60。
type countingBuilder struct {
	base           int
	lastInput      prompt.Input
	builds         int
	collapseCalled bool
}

func (b *countingBuilder) Build(in prompt.Input) prompt.BuildResult {
	b.builds++
	b.lastInput = cloneInput(in) // 记录副本，供断言「不改调用方数据」
	total := b.base
	total += len(in.MemoryTail) * 20
	total += len(in.ActiveTools) * 40
	coreCount := 0
	for _, t := range in.Tools {
		if t.Tier == prompt.TierCore {
			coreCount++
		}
	}
	if coreCount > prompt.CoreToolsFloor {
		total += (coreCount - prompt.CoreToolsFloor) * 50
	}
	for _, m := range in.History {
		if m.Role == llm.RoleTool && !strings.HasPrefix(m.Content, "[tool:") {
			total += 60
		}
	}
	return prompt.BuildResult{Breakdown: prompt.Breakdown{Total: total}}
}

func mkCoreTools(n int) []prompt.ToolRef {
	out := make([]prompt.ToolRef, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, prompt.ToolRef{
			Name:        "tool" + string(rune('a'+i)),
			Description: "d",
			Tier:        prompt.TierCore,
		})
	}
	return out
}

func bigHistory(toolMsgs int) []llm.Message {
	out := []llm.Message{{Role: llm.RoleUser, Content: "q"}}
	for i := 0; i < toolMsgs; i++ {
		out = append(out, llm.Message{Role: llm.RoleAssistant, Content: "calling",
			ToolCalls: []llm.ToolCall{{ID: "c" + string(rune('0'+i)), Name: "tool" + string(rune('0'+i))}}})
		out = append(out, llm.Message{Role: llm.RoleTool, ToolCallID: "c" + string(rune('0'+i)),
			Content: strings.Repeat("x", 300)})
	}
	return out
}

// TestGateWithinBudgetPassthrough 预算内直通：零降级、零额外 Build。
// 计费：base100 + 记忆20 + 提升2*40 + 超额core(8-4)*50 + tool结果6*60 = 760 < 1000。
func TestGateWithinBudgetPassthrough(t *testing.T) {
	inner := &countingBuilder{base: 100}
	g := NewBudgetGate(inner, 1000, 2)
	res := g.Build(prompt.Input{
		MemoryTail: "tail", ActiveTools: []string{"a", "b"},
		Tools: mkCoreTools(8), History: bigHistory(6),
	})
	if res.OverBudget {
		t.Fatalf("预算内不应报超限")
	}
	if len(res.Degrade) != 0 {
		t.Fatalf("预算内不应有降级动作: %v", res.Degrade)
	}
	if inner.builds != 1 {
		t.Fatalf("预算内应只 Build 一次，实际 %d", inner.builds)
	}
}

// TestGateUnlimitedPassthrough 不限预算（max_total=0）直通，闸门零开销。
func TestGateUnlimitedPassthrough(t *testing.T) {
	inner := &countingBuilder{base: 1 << 30}
	g := NewBudgetGate(inner, 0, 2)
	res := g.Build(prompt.Input{MemoryTail: "tail"})
	if res.OverBudget || len(res.Degrade) != 0 {
		t.Fatalf("不限预算不应降级: over=%v degrade=%v", res.OverBudget, res.Degrade)
	}
}

// TestGateDegradeOrder 五级按序降级：构造逐档刚好省够的计数桩，验证顺序与动作记录。
// 计费：base100 + 记忆20 + 提升40/条 + 超额core50/条 + 未折叠tool结果60/条。
// 初始 700 → ①680 → ②640 → ③540 → ④480 ≤ 500 收敛，恰走 ①②③④ 四档。
func TestGateDegradeOrder(t *testing.T) {
	inner := &countingBuilder{base: 100}
	g := NewBudgetGate(inner, 500, 2)
	res := g.Build(prompt.Input{
		MemoryTail:  "mem",                   // 20
		ActiveTools: []string{"a", "b", "c"}, // 120
		Tools:       mkCoreTools(6),          // (6-4)*50 = 100
		History:     bigHistory(6),           // 6*60 = 360（④可折叠 1 条）
	})
	if res.OverBudget {
		t.Fatalf("五档足够省时应降回预算内，不应报超限")
	}
	if len(res.Degrade) == 0 {
		t.Fatalf("超预算必须记录降级动作")
	}
	// 档位严格递增
	for i := 1; i < len(res.Degrade); i++ {
		if res.Degrade[i].Level <= res.Degrade[i-1].Level {
			t.Fatalf("降级档位必须严格递增: %v", res.Degrade)
		}
	}
	if res.Degrade[0].Level != prompt.LevelDropMemory {
		t.Fatalf("第①级必须先省记忆，实际 %v", res.Degrade[0])
	}
	if len(res.Degrade) != 4 {
		t.Fatalf("应恰走 ①②③④ 四档，实际 %d: %v", len(res.Degrade), res.Degrade)
	}
	// 每档都有 Detail（可观测铁律）
	for _, a := range res.Degrade {
		if a.Detail == "" {
			t.Fatalf("档位 %v 缺少动作明细", a.Level)
		}
	}
}

// TestGateExhaustedExplicitError 五级用尽仍超 → OverBudget 明确上报，绝不静默放行。
func TestGateExhaustedExplicitError(t *testing.T) {
	inner := &countingBuilder{base: 10_000} // 降级省的那点永远不够
	g := NewBudgetGate(inner, 100, 2)
	res := g.Build(prompt.Input{
		MemoryTail:  "mem",
		ActiveTools: []string{"a", "b", "c", "d", "e"},
		Tools:       mkCoreTools(8),
		History:     bigHistory(10),
	})
	if !res.OverBudget {
		t.Fatalf("五级用尽仍超预算必须置 OverBudget（严禁静默放行）")
	}
	// ①②③④ 全部可用（⑤无压缩器不可用）→ 恰好 4 条动作
	if len(res.Degrade) != 4 {
		t.Fatalf("应记录 4 条可用降级动作，实际 %d: %v", len(res.Degrade), res.Degrade)
	}
}

// TestGateNoDegradeWhenNothingAvailable 没有任何可降级对象时，
// 第一次规划即 Exhausted → 直接明确报错（不做无意义重建）。
func TestGateNoDegradeWhenNothingAvailable(t *testing.T) {
	inner := &countingBuilder{base: 10_000}
	g := NewBudgetGate(inner, 100, 4)
	buildsBefore := inner.builds
	res := g.Build(prompt.Input{Tools: mkCoreTools(4)}) // core=4 不可再砍，无记忆/历史/提升
	if !res.OverBudget {
		t.Fatalf("无任何降级对象仍超预算必须明确报错")
	}
	if len(res.Degrade) != 0 {
		t.Fatalf("无可用动作时不应记录动作: %v", res.Degrade)
	}
	if inner.builds != buildsBefore+1 {
		t.Fatalf("无可用动作时应只重建一次，实际 builds=%d", inner.builds-buildsBefore)
	}
}

// TestGateDoesNotMutateCallerInput 降级不得污染调用方 Input（引擎跨步复用 history）。
func TestGateDoesNotMutateCallerInput(t *testing.T) {
	inner := &countingBuilder{base: 10_000}
	g := NewBudgetGate(inner, 100, 2)
	in := prompt.Input{
		MemoryTail:  "mem",
		ActiveTools: []string{"a", "b", "c"},
		Tools:       mkCoreTools(8),
		History:     bigHistory(8),
	}
	_ = g.Build(in)
	if in.MemoryTail != "mem" {
		t.Fatalf("调用方 MemoryTail 被改写: %q", in.MemoryTail)
	}
	if len(in.ActiveTools) != 3 {
		t.Fatalf("调用方 ActiveTools 被改写: %d", len(in.ActiveTools))
	}
	if n := countCoreTools(in.Tools); n != 8 {
		t.Fatalf("调用方 Tools 被改写: core=%d", n)
	}
	for _, m := range in.History {
		if m.Role == llm.RoleTool && strings.HasPrefix(m.Content, "[tool:") {
			t.Fatalf("调用方 History 被原地折叠")
		}
	}
}

// TestGateTopKFloor 第②级减半不得低于 MinTopK（按需工具不能被砍到不可见）。
// 计费：base100 + 5*40=300 > 240 → ①无记忆跳过 → ②halve(5)=2 → 180 ≤ 240 收敛。
func TestGateTopKFloor(t *testing.T) {
	inner2 := &countingBuilder{base: 100}
	g2 := NewBudgetGate(inner2, 240, 2)
	in := prompt.Input{ActiveTools: []string{"a", "b", "c", "d", "e"}}
	res := g2.Build(in)
	if res.OverBudget {
		t.Fatalf("应收敛")
	}
	last := res.Degrade[len(res.Degrade)-1]
	if last.Level != prompt.LevelHalveTopK {
		t.Fatalf("应停在②级，实际 %v", last.Level)
	}
	if len(inner2.lastInput.ActiveTools) != 2 {
		t.Fatalf("topK 应减半到下限 2，实际 %d", len(inner2.lastInput.ActiveTools))
	}
}

// TestCollapseToolResults 第④级：保留最近 keep 条，旧长结果折叠为引用，短结果保留。
func TestCollapseToolResults(t *testing.T) {
	history := []llm.Message{
		{Role: llm.RoleUser, Content: "q"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "fs.read"}}},
		{Role: llm.RoleTool, ToolCallID: "c1", Content: strings.Repeat("长结果", 100)},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c2", Name: "cmd"}}},
		{Role: llm.RoleTool, ToolCallID: "c2", Content: "短"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c3", Name: "web"}}},
		{Role: llm.RoleTool, ToolCallID: "c3", Content: strings.Repeat("y", 250)},
	}
	// keep=2：最近 2 条 tool 消息（c2 短、c3 长）保留；c1 折叠。
	n := collapseToolResults(history, 2, 200)
	if n != 1 {
		t.Fatalf("应折叠 1 条，实际 %d", n)
	}
	if !strings.Contains(history[2].Content, "[tool:fs.read") {
		t.Fatalf("c1 应折叠为带工具名的引用: %q", history[2].Content)
	}
	if history[4].Content != "短" {
		t.Fatalf("短结果不应折叠: %q", history[4].Content)
	}
	if strings.HasPrefix(history[6].Content, "[tool:") {
		t.Fatalf("最近 keep 条内的长结果不应折叠")
	}
}

// TestCollapseBoundaryInsufficient 不足 keep 条 tool 消息时无可折叠对象。
func TestCollapseBoundaryInsufficient(t *testing.T) {
	history := []llm.Message{
		{Role: llm.RoleTool, ToolCallID: "c1", Content: strings.Repeat("x", 300)},
	}
	if collapseBoundary(history, 4) != -1 {
		t.Fatalf("不足 keep 条应返回 -1")
	}
	if countCollapsible(history, 4, 200) != 0 {
		t.Fatalf("不足 keep 条不应有可折叠对象")
	}
}

// TestTrimCoreTools 裁剪保序：core 截到 keep 条，deferred/on_demand 不动。
func TestTrimCoreTools(t *testing.T) {
	tools := append(mkCoreTools(6),
		prompt.ToolRef{Name: "d1", Tier: prompt.TierDeferred},
		prompt.ToolRef{Name: "od1", Tier: prompt.TierOnDemand})
	out := trimCoreTools(tools, 4)
	if got := countCoreTools(out); got != 4 {
		t.Fatalf("core 应裁到 4，实际 %d", got)
	}
	if out[4].Name != "d1" || out[5].Name != "od1" {
		t.Fatalf("非 core 档必须原位保留: %+v", out[4:])
	}
	// 裁掉的是排尾的 core（注册顺序 = 推荐顺序）
	if out[0].Name != "toola" || out[3].Name != "toold" {
		t.Fatalf("core 保序失败: %v", out[:4])
	}
}

// TestDegradeSummary 汇总文本非空且含档名。
func TestDegradeSummary(t *testing.T) {
	s := DegradeSummary([]prompt.DegradeAction{
		{Level: prompt.LevelDropMemory, Name: prompt.LevelName(prompt.LevelDropMemory), Detail: "省略 L2/L3 记忆段"},
	})
	if s == "" || !strings.Contains(s, "省略记忆层") {
		t.Fatalf("汇总应含档名: %q", s)
	}
	if DegradeSummary(nil) != "" {
		t.Fatalf("空动作汇总应为空串")
	}
}
