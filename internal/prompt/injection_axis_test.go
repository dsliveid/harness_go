package prompt

import (
	"fmt"
	"strings"
	"testing"
)

// TestDeferredOverflowIsObservable 索引段被上限截断时必须留下痕迹。
//
// 与 SOPOverflow 同口径：绝不静默丢弃。改造前这里是裸切片截断，
// 被丢弃多少完全不可见 —— 模型对这批工具"连名字都看不到"，
// 而运维/前端也拿不到任何信号。
func TestDeferredOverflowIsObservable(t *testing.T) {
	b := NewBuilder(nil, Limits{MaxDeferredIndex: 2})
	tools := []ToolRef{
		{Name: "d1", Description: "一", Tier: TierDeferred},
		{Name: "d2", Description: "二", Tier: TierDeferred},
		{Name: "d3", Description: "三", Tier: TierDeferred},
		{Name: "d4", Description: "四", Tier: TierDeferred},
	}
	res := b.Build(Input{Tools: tools})
	if len(res.IndexedDeferred) != 2 {
		t.Fatalf("索引应只保留 2 条，实际 %d", len(res.IndexedDeferred))
	}
	if res.DeferredOverflow != 2 {
		t.Fatalf("被丢弃条数应可见且为 2，实际 %d", res.DeferredOverflow)
	}
}

// TestDeferredOverflowZeroWhenWithinLimit 未超限时不得报溢出（避免假告警）。
func TestDeferredOverflowZeroWhenWithinLimit(t *testing.T) {
	b := NewBuilder(nil, Limits{MaxDeferredIndex: 10})
	res := b.Build(Input{Tools: []ToolRef{
		{Name: "d1", Description: "一", Tier: TierDeferred},
	}})
	if res.DeferredOverflow != 0 {
		t.Fatalf("未超限不应报溢出，实际 %d", res.DeferredOverflow)
	}
}

// TestIndexSectionPointsToLookupTool 索引段文案必须指明 lookup_tool 这个加载入口。
//
// 改造前文案是「需要时请显式加载」，但全仓并无任何加载工具 ——
// 那句承诺无法兑现，模型只能盲猜参数。文案与实现必须同时成立。
func TestIndexSectionPointsToLookupTool(t *testing.T) {
	b := NewBuilder(nil, Limits{})
	res := b.Build(Input{Tools: []ToolRef{
		{Name: "create_plan", Description: "建计划", Tier: TierDeferred},
	}})
	sys := res.Request.System
	if !strings.Contains(sys, "lookup_tool") {
		t.Fatalf("索引段必须指引 lookup_tool: %q", sys)
	}
	if strings.Contains(sys, "需要时请显式加载") {
		t.Fatalf("不应保留无法兑现的旧文案: %q", sys)
	}
}

// TestMemoryInjectionAxisIsFrozenWithinRun 同一份 Input 反复 Build 结果必须完全一致。
//
// 这是"注入快照在 Run 边界冻结"的可验证形态：引擎在整个 Run 内复用
// 同一个 MemoryCore/MemoryTail（req 字段），不会中途重新召回。
// 若 builder 自身对相同输入产出不同输出，冻结就无从谈起（缓存也会失效）。
func TestMemoryInjectionAxisIsFrozenWithinRun(t *testing.T) {
	b := NewBuilder(nil, Limits{})
	in := Input{
		SystemBase: "base",
		MemoryCore: "## 核心记忆\n- L0 约束",
		MemoryTail: "## 相关记忆\n- [L2] 某结论",
		Tools:      []ToolRef{{Name: "read_file", Tier: TierCore}},
		UserInput:  "问题",
	}
	first := b.Build(in)
	for i := 0; i < 3; i++ {
		again := b.Build(in)
		if again.Request.System != first.Request.System {
			t.Fatalf("第 %d 次重建 system 不一致（注入快照未冻结）", i+2)
		}
		if len(again.Request.Messages) != len(first.Request.Messages) {
			t.Fatalf("第 %d 次重建消息数不一致", i+2)
		}
		last := again.Request.Messages[len(again.Request.Messages)-1]
		want := first.Request.Messages[len(first.Request.Messages)-1]
		if last.Content != want.Content {
			t.Fatalf("第 %d 次重建动态尾段不一致:\n got=%q\nwant=%q", i+2, last.Content, want.Content)
		}
	}
}

// TestMemoryCoreInStablePrefix MemoryCore 必须在 deferred 索引段之前（前缀可缓存）。
//
// 顺序错了会让"每条索引变化"都导致核心记忆之后的全部前缀失效。
func TestMemoryCoreInStablePrefix(t *testing.T) {
	b := NewBuilder(nil, Limits{})
	res := b.Build(Input{
		SystemBase: "BASE",
		MemoryCore: "COREMEM",
		Tools:      []ToolRef{{Name: "d1", Description: "x", Tier: TierDeferred}},
	})
	sys := res.Request.System
	coreIdx := strings.Index(sys, "COREMEM")
	indexIdx := strings.Index(sys, "可按需调用的工具")
	if coreIdx < 0 || indexIdx < 0 {
		t.Fatalf("两段都应存在: %q", sys)
	}
	if coreIdx > indexIdx {
		t.Fatalf("核心记忆必须在索引段之前（否则前缀缓存被索引变化击穿）: core@%d index@%d", coreIdx, indexIdx)
	}
}

// TestMemoryTailGoesToUserMessage MemoryTail 必须进 user 消息（置尾），绝不进 system。
//
// 进 system 会让"每轮变化"的动态内容污染稳定前缀，前缀缓存全废。
func TestMemoryTailGoesToUserMessage(t *testing.T) {
	b := NewBuilder(nil, Limits{})
	res := b.Build(Input{
		SystemBase: "BASE",
		MemoryTail: "TAILMARK",
		UserInput:  "问题",
	})
	if strings.Contains(res.Request.System, "TAILMARK") {
		t.Fatal("动态尾段不得进 system")
	}
	last := res.Request.Messages[len(res.Request.Messages)-1]
	if !strings.Contains(last.Content, "TAILMARK") {
		t.Fatalf("动态尾段应置尾到 user 消息: %q", last.Content)
	}
}

// TestToolTierSplit 三档分流必须准确（core 全量 / deferred 索引 / on_demand 零注入）。
func TestToolTierSplit(t *testing.T) {
	b := NewBuilder(nil, Limits{})
	res := b.Build(Input{Tools: []ToolRef{
		{Name: "c1", Tier: TierCore},
		{Name: "d1", Tier: TierDeferred, Description: "deferred 工具"},
		{Name: "o1", Tier: TierOnDemand},
	}})
	if fmt.Sprint(res.InjectedCore) != "[c1]" {
		t.Fatalf("core 注入不符: %v", res.InjectedCore)
	}
	if fmt.Sprint(res.IndexedDeferred) != "[d1]" {
		t.Fatalf("deferred 索引不符: %v", res.IndexedDeferred)
	}
	if fmt.Sprint(res.SkippedOnDemand) != "[o1]" {
		t.Fatalf("on_demand 应零注入: %v", res.SkippedOnDemand)
	}
	if strings.Contains(res.Request.System, "o1") {
		t.Fatal("on_demand 工具名不应出现在 system 中")
	}
}
