package prompt

import (
	"strings"
	"testing"

	"harness-go/internal/tokenizer"
)

// D-2 核心断言：命中的 deferred 工具必须从「只进索引」提升为「全量定义注入」。
func TestActiveToolsPromotedToFullDefinition(t *testing.T) {
	b := NewBuilder(tokenizer.NewHeuristic(), DefaultLimits())
	in := baseInput()
	in.ActiveTools = []string{"web_fetch"}
	res := b.Build(in)

	// web_fetch 原本是 deferred，命中后应出现在 tools 数组（全量 schema）。
	var found bool
	for _, tl := range res.Request.Tools {
		if tl.Name == "web_fetch" {
			found = true
			if tl.Description == "" {
				t.Fatal("提升后应携带完整描述")
			}
		}
	}
	if !found {
		t.Fatalf("web_fetch 应被提升为全量注入，实际 tools=%+v", res.Request.Tools)
	}

	// 提升后不得再出现在索引段（避免重复注入，浪费 token）。
	for _, n := range res.IndexedDeferred {
		if n == "web_fetch" {
			t.Fatal("已提升的工具不应再进索引段")
		}
	}
	// plan 未被命中，仍应只进索引。
	var planIndexed bool
	for _, n := range res.IndexedDeferred {
		if n == "plan" {
			planIndexed = true
		}
	}
	if !planIndexed {
		t.Fatal("未命中的 deferred 工具应保持只进索引")
	}

	// PromotedTools 记录命中项（供 /tools/injection 展示来源）。
	if len(res.PromotedTools) != 1 || res.PromotedTools[0] != "web_fetch" {
		t.Fatalf("PromotedTools 期望 [web_fetch]，得到 %+v", res.PromotedTools)
	}
}

// 宽进严出：未命中的 deferred 工具仍可执行（注入层不做禁用），
// 且 ActiveTools 里的未知名字必须静默忽略（索引可能过期，不得报错）。
func TestActiveToolsUnknownNamesIgnored(t *testing.T) {
	b := NewBuilder(tokenizer.NewHeuristic(), DefaultLimits())
	in := baseInput()
	in.ActiveTools = []string{"not_registered", "", "  "}
	res := b.Build(in)

	// 未知名字不产生任何注入，也不得 panic / 报错。
	if len(res.PromotedTools) != 0 {
		t.Fatalf("未知工具名不应被提升，得到 %+v", res.PromotedTools)
	}
	// core 仍是 2 个，deferred 仍是 2 个（plan/web_fetch 都还在索引里）。
	if len(res.Request.Tools) != 2 {
		t.Fatalf("未知名字不应改变 core 注入数，得到 %d", len(res.Request.Tools))
	}
	if len(res.IndexedDeferred) != 2 {
		t.Fatalf("deferred 应仍是 2 条索引，得到 %v", res.IndexedDeferred)
	}
}

// ActiveTools 为空时行为必须与 D-2 之前完全一致（零回归）。
func TestActiveToolsEmptyKeepsLegacyBehavior(t *testing.T) {
	b := NewBuilder(tokenizer.NewHeuristic(), DefaultLimits())
	res := b.Build(baseInput()) // 不设 ActiveTools

	if len(res.PromotedTools) != 0 {
		t.Fatalf("未设 ActiveTools 不应有提升项，得到 %+v", res.PromotedTools)
	}
	if len(res.Request.Tools) != 2 {
		t.Fatalf("core 应为 2 个，得到 %d", len(res.Request.Tools))
	}
	if len(res.IndexedDeferred) != 2 {
		t.Fatalf("deferred 应为 2 个，得到 %v", res.IndexedDeferred)
	}
}

// 提升不得让 on_demand 档意外进入注入（三档边界必须严格）。
func TestActiveToolsCannotPromoteOnDemand(t *testing.T) {
	b := NewBuilder(tokenizer.NewHeuristic(), DefaultLimits())
	in := baseInput()
	// browser_click 是 on_demand 档：即使被检索命中也不得注入全量定义。
	in.ActiveTools = []string{"browser_click"}
	res := b.Build(in)

	for _, tl := range res.Request.Tools {
		if tl.Name == "browser_click" {
			t.Fatal("on_demand 档不得被提升为全量注入（D11 三档边界）")
		}
	}
	if len(res.PromotedTools) != 0 {
		t.Fatalf("on_demand 不应出现在 PromotedTools，得到 %+v", res.PromotedTools)
	}
	for _, n := range res.SkippedOnDemand {
		if n == "browser_click" {
			return // 正确：仍在 on_demand 未注入集合中
		}
	}
	t.Fatal("browser_click 应仍在 SkippedOnDemand 中")
}

// 提升的工具名必须计入 InjectedCore（它是「全量注入集合」的权威清单）。
func TestPromotedIncludedInInjectedCore(t *testing.T) {
	b := NewBuilder(tokenizer.NewHeuristic(), DefaultLimits())
	in := baseInput()
	in.ActiveTools = []string{"web_fetch", "plan"}
	res := b.Build(in)

	got := strings.Join(res.InjectedCore, ",")
	if !strings.Contains(got, "web_fetch") || !strings.Contains(got, "plan") {
		t.Fatalf("InjectedCore 应含提升项，得到 %s", got)
	}
	if len(res.InjectedCore) != 4 { // 2 core + 2 promoted
		t.Fatalf("InjectedCore 期望 4 项，得到 %d：%v", len(res.InjectedCore), res.InjectedCore)
	}
	if len(res.IndexedDeferred) != 0 {
		t.Fatalf("全部 deferred 已提升，索引段应为空，得到 %v", res.IndexedDeferred)
	}
}
