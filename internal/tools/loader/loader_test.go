package loader

import (
	"strings"
	"testing"

	"harness-go/pkg/toolapi"
)

func spec(name string, tier toolapi.Tier, desc string) toolapi.Spec {
	return toolapi.Spec{Name: name, Description: desc, Tier: tier}.Normalized()
}

func TestBuildSplitsByTier(t *testing.T) {
	inj := Build([]toolapi.Spec{
		spec("a", toolapi.TierCore, "基础工具"),
		spec("b", toolapi.TierDeferred, "按需工具"),
		spec("c", toolapi.TierOnDemand, "手动工具"),
	}, Limits{CoreSoft: 8, DeferredSoft: 60})

	if len(inj.Core) != 1 || len(inj.DeferredIndex) != 1 || len(inj.OnDemand) != 1 {
		t.Fatalf("分层错误: core=%d deferred=%d on_demand=%d", len(inj.Core), len(inj.DeferredIndex), len(inj.OnDemand))
	}
	if inj.CostIndex < IndexCostPerTool {
		t.Fatalf("索引成本应 ≥ 基线 %d，得到 %d", IndexCostPerTool, inj.CostIndex)
	}
	if inj.CostTotal != inj.CostCore+inj.CostIndex {
		t.Fatalf("总成本应为两段之和: %d vs %d", inj.CostTotal, inj.CostCore+inj.CostIndex)
	}
	lines := inj.IndexLines()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "- b — ") {
		t.Fatalf("索引行格式异常: %v", lines)
	}
	if names := inj.Names(toolapi.TierOnDemand); len(names) != 1 || names[0] != "c" {
		t.Fatalf("Names(on_demand) 异常: %v", names)
	}
}

func TestDeferredSoftLimitTruncates(t *testing.T) {
	specs := []toolapi.Spec{
		spec("d1", toolapi.TierDeferred, "一"),
		spec("d2", toolapi.TierDeferred, "二"),
		spec("d3", toolapi.TierDeferred, "三"),
	}
	inj := Build(specs, Limits{CoreSoft: 8, DeferredSoft: 2})
	if len(inj.DeferredIndex) != 2 {
		t.Fatalf("索引应被截断为 2 条，得到 %d", len(inj.DeferredIndex))
	}
	if len(inj.DeferredHidden) != 1 || inj.DeferredHidden[0] != "d3" {
		t.Fatalf("被截断工具应记录，得到 %v", inj.DeferredHidden)
	}
	if len(inj.Warnings) == 0 {
		t.Fatal("超软上限应产生告警")
	}
}

func TestCoreSoftWarnOnly(t *testing.T) {
	inj := Build([]toolapi.Spec{
		spec("a", toolapi.TierCore, "x"),
		spec("b", toolapi.TierCore, "y"),
	}, Limits{CoreSoft: 1, DeferredSoft: 60})
	if len(inj.Core) != 2 {
		t.Fatalf("core 超软上限不应被裁剪，得到 %d", len(inj.Core))
	}
	if len(inj.Warnings) == 0 {
		t.Fatal("core 超软上限应告警")
	}
}

func TestIndexDescriptionTruncated(t *testing.T) {
	long := strings.Repeat("很长的描述", 40)
	inj := Build([]toolapi.Spec{spec("x", toolapi.TierDeferred, long)}, Limits{})
	got := []rune(inj.DeferredIndex[0].Description)
	if len(got) > MaxIndexDescription+1 {
		t.Fatalf("描述应截断到 %d 字，得到 %d", MaxIndexDescription, len(got))
	}
}
