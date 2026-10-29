package prompt

import (
	"encoding/json"
	"strings"
	"testing"

	"harness-go/internal/llm"
	"harness-go/internal/tokenizer"
)

func testTools() []ToolRef {
	return []ToolRef{
		{Name: "read_file", Description: "读取文件", Tier: TierCore, InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "edit_file", Description: "精确替换", Tier: TierCore, InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "plan", Description: "计划管理\n支持多步计划文档", Tier: TierDeferred},
		{Name: "web_fetch", Description: "抓取网页", Tier: TierDeferred},
		{Name: "browser_click", Description: "点击元素", Tier: TierOnDemand},
	}
}

func baseInput() Input {
	return Input{
		Model:          "gpt-4o",
		SystemBase:     "你是一个编码助手。",
		SOP:            []string{"修改前先读文件", "不要执行破坏性命令"},
		ProjectSummary: "Go + React 单体二进制项目",
		Tools:          testTools(),
		UserInput:      "帮我看下 main.go",
	}
}

// 核心断言：core 全量进 tools、deferred 只进索引、on_demand 完全不出现。
func TestTierLayering(t *testing.T) {
	b := NewBuilder(tokenizer.NewHeuristic(), DefaultLimits())
	res := b.Build(baseInput())

	// core 进 tools 数组
	if len(res.Request.Tools) != 2 {
		t.Fatalf("core 工具应全量注入 2 个，得到 %d", len(res.Request.Tools))
	}
	names := map[string]bool{}
	for _, tl := range res.Request.Tools {
		names[tl.Name] = true
	}
	if !names["read_file"] || !names["edit_file"] {
		t.Fatalf("core 工具缺失: %+v", names)
	}

	// deferred 只在 system 索引段出现，且不带 schema
	sys := res.Request.System
	if !strings.Contains(sys, "可按需调用的工具") {
		t.Fatalf("system 应含 deferred 索引段: %s", sys)
	}
	if !strings.Contains(sys, "plan") || !strings.Contains(sys, "web_fetch") {
		t.Fatalf("deferred 工具名应出现在索引: %s", sys)
	}
	if strings.Contains(sys, `"type":"object"`) {
		t.Fatalf("deferred 不应注入 schema: %s", sys)
	}
	// 索引只取描述首行
	if strings.Contains(sys, "支持多步计划文档") {
		t.Fatalf("索引应只取描述首行: %s", sys)
	}

	// on_demand 完全不出现
	if strings.Contains(sys, "browser_click") {
		t.Fatalf("on_demand 不应出现在 system: %s", sys)
	}
	for _, tl := range res.Request.Tools {
		if tl.Name == "browser_click" {
			t.Fatal("on_demand 不应进 tools 数组")
		}
	}

	// 注入清单可观测
	if len(res.InjectedCore) != 2 || len(res.IndexedDeferred) != 2 || len(res.SkippedOnDemand) != 1 {
		t.Fatalf("注入清单错误: core=%v deferred=%v on_demand=%v",
			res.InjectedCore, res.IndexedDeferred, res.SkippedOnDemand)
	}
	if res.SkippedOnDemand[0] != "browser_click" {
		t.Fatalf("skipped 清单错误: %v", res.SkippedOnDemand)
	}
}

// 前缀稳定：同一输入两次组装，system 必须逐字节一致（保护 provider 前缀缓存）。
func TestSystemPrefixIsStable(t *testing.T) {
	b := NewBuilder(tokenizer.NewHeuristic(), DefaultLimits())
	in := baseInput()

	first := b.Build(in)
	// 改变动态段（模拟检索结果变化）与用户输入，system 不得变化
	in.DynamicTail = "检索命中：read_file 定义在 internal/plugins/fs/read.go"
	in.UserInput = "换个问题"
	second := b.Build(in)

	if first.Request.System != second.Request.System {
		t.Fatalf("动态内容变化不应影响 system（前缀缓存会失效）:\nfirst=%s\nsecond=%s",
			first.Request.System, second.Request.System)
	}
}

// 动态内容必须置尾：不能进 system，必须在最后一条 user 消息里。
func TestDynamicTailGoesLast(t *testing.T) {
	b := NewBuilder(tokenizer.NewHeuristic(), DefaultLimits())
	in := baseInput()
	in.DynamicTail = "DYNAMIC_MARKER"
	res := b.Build(in)

	if strings.Contains(res.Request.System, "DYNAMIC_MARKER") {
		t.Fatal("动态内容不得进入 system")
	}
	msgs := res.Request.Messages
	last := msgs[len(msgs)-1]
	if last.Role != llm.RoleUser {
		t.Fatalf("最后一条应为 user，得到 %s", last.Role)
	}
	if !strings.HasPrefix(last.Content, "DYNAMIC_MARKER") {
		t.Fatalf("动态内容应在最后一条 user 消息开头: %s", last.Content)
	}
	if !strings.Contains(last.Content, "帮我看下 main.go") {
		t.Fatalf("用户输入应保留: %s", last.Content)
	}
}

// SOP 全量注入 + 超上限截断（二次校验）。
func TestSOPFullInjectionAndCap(t *testing.T) {
	b := NewBuilder(tokenizer.NewHeuristic(), DefaultLimits())
	in := baseInput()
	res := b.Build(in)
	if !strings.Contains(res.Request.System, "项目规范（SOP）") {
		t.Fatalf("SOP 应全量注入: %s", res.Request.System)
	}
	if !strings.Contains(res.Request.System, "修改前先读文件") {
		t.Fatalf("SOP 内容缺失: %s", res.Request.System)
	}

	// 超过 6 条只保留前 6
	in.SOP = []string{"s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8"}
	res = b.Build(in)
	if strings.Contains(res.Request.System, "s7") || strings.Contains(res.Request.System, "s8") {
		t.Fatalf("SOP 应截断到上限 6 条: %s", res.Request.System)
	}
	if !strings.Contains(res.Request.System, "s6") {
		t.Fatalf("第 6 条应保留: %s", res.Request.System)
	}
}

// 四段明细：各段非负且 Total = 各段之和。
func TestBreakdownSums(t *testing.T) {
	b := NewBuilder(tokenizer.NewHeuristic(), DefaultLimits())
	res := b.Build(baseInput())

	bd := res.Breakdown
	if bd.System <= 0 || bd.Tools <= 0 || bd.Input <= 0 {
		t.Fatalf("system/tools/input 段应为正数: %+v", bd)
	}
	if bd.Total != bd.System+bd.History+bd.Tools+bd.Input {
		t.Fatalf("Total 应等于四段之和: %+v", bd)
	}
	if !res.Estimated {
		t.Fatal("heuristic 计数应标记 Estimated")
	}
}

// 历史裁剪：超预算时从最旧丢弃，保留最近消息。
func TestHistoryTrimKeepsRecent(t *testing.T) {
	b := NewBuilder(tokenizer.NewHeuristic(), Limits{MaxHistory: 30})
	in := baseInput()
	in.History = []llm.Message{
		{Role: llm.RoleUser, Content: strings.Repeat("旧", 40)},
		{Role: llm.RoleAssistant, Content: strings.Repeat("旧", 40)},
		{Role: llm.RoleUser, Content: "最近的问题"},
	}
	res := b.Build(in)

	if res.DroppedHistory == 0 {
		t.Fatalf("应裁剪掉部分历史，实际未裁剪（history tokens=%d）", res.Breakdown.History)
	}
	// 最后一条历史（最近的）必须保留
	kept := res.Request.Messages
	if len(kept) < 2 {
		t.Fatalf("至少应保留最近历史 + 本轮输入，实际 %d 条", len(kept))
	}
	last := kept[len(kept)-2]
	if last.Content != "最近的问题" {
		t.Fatalf("应保留最近的历史消息，得到 %q", last.Content)
	}
}

// deferred 索引超上限时截断。
func TestDeferredIndexCap(t *testing.T) {
	tools := make([]ToolRef, 0, 80)
	for i := 0; i < 80; i++ {
		tools = append(tools, ToolRef{Name: "t" + string(rune('a'+i%26)) + string(rune('0'+i%10)), Tier: TierDeferred})
	}
	b := NewBuilder(tokenizer.NewHeuristic(), Limits{MaxDeferredIndex: 5})
	res := b.Build(Input{SystemBase: "x", Tools: tools, UserInput: "hi"})
	if len(res.IndexedDeferred) != 5 {
		t.Fatalf("deferred 索引应截断到 5 条，得到 %d", len(res.IndexedDeferred))
	}
}

// 超总预算时明确置标志（禁止静默截断）。
func TestOverBudgetFlag(t *testing.T) {
	b := NewBuilder(tokenizer.NewHeuristic(), Limits{MaxTotal: 10})
	res := b.Build(baseInput())
	if !res.OverBudget {
		t.Fatalf("应标记 OverBudget（total=%d, limit=10）", res.Breakdown.Total)
	}

	b2 := NewBuilder(tokenizer.NewHeuristic(), Limits{MaxTotal: 100000})
	if b2.Build(baseInput()).OverBudget {
		t.Fatal("预算充足时不应标记 OverBudget")
	}
}

// 未指定 tier 的工具按 deferred 处理（保守：不占全量成本）。
func TestUnknownTierDefaultsToDeferred(t *testing.T) {
	b := NewBuilder(tokenizer.NewHeuristic(), DefaultLimits())
	res := b.Build(Input{
		SystemBase: "x",
		Tools:      []ToolRef{{Name: "mystery", Description: "未知档位"}},
		UserInput:  "hi",
	})
	if len(res.Request.Tools) != 0 {
		t.Fatalf("未知档位不应进 tools 数组: %+v", res.Request.Tools)
	}
	if len(res.IndexedDeferred) != 1 {
		t.Fatalf("未知档位应按 deferred 处理: %+v", res.IndexedDeferred)
	}
}
