package service

import (
	"context"
	"strings"
	"testing"

	"harness-go/internal/sop"
)

// newTestSOPService 建一个临时 dataDir + 可控开关来源的 SOPService。
func newTestSOPService(t *testing.T, disabled []string, planMode string) *SOPService {
	t.Helper()
	svc := NewSOPService(t.TempDir(), 800)
	svc.SetDisabledProvider(func(context.Context) []string { return disabled })
	svc.SetPlanModeProvider(func(context.Context) string { return planMode })
	return svc
}

// TestSOPServiceRulesRespectDisabling 用户关掉的规则不得出现在 Rules 里，硬规则恒生效。
func TestSOPServiceRulesRespectDisabling(t *testing.T) {
	ctx := context.Background()

	all := newTestSOPService(t, nil, sop.PlanModeStandard).Rules(ctx, sop.Context{SessionID: "s"})
	if len(all) == 0 {
		t.Fatal("主会话应至少渲染一条规则")
	}
	hasTitle := func(rules []string, title string) bool {
		for _, r := range rules {
			if strings.Contains(r, "【"+title+"】") {
				return true
			}
		}
		return false
	}

	// ① 关掉「代码克制与精确修改规范」→ 主会话少一条，且该标题消失
	var softID, softTitle string
	for _, def := range sop.Definitions() {
		if !def.Item.IsHardRule && def.Item.Scope == sop.ScopeUniversal {
			softID, softTitle = def.Item.ID, def.Variants[0].Title
			break
		}
	}
	if softID == "" {
		t.Fatal("目录里应存在可关闭的通用规则")
	}
	filtered := newTestSOPService(t, []string{softID}, sop.PlanModeStandard).Rules(ctx, sop.Context{SessionID: "s"})
	if len(filtered)+1 != len(all) {
		t.Fatalf("关闭一条后应恰好少一条（%d → %d）", len(all), len(filtered))
	}
	if hasTitle(filtered, softTitle) {
		t.Fatalf("已关闭规则 %s 仍被注入", softID)
	}

	// ② 关掉硬规则（即便强行写进来）→ 规则数不变（安全边界不可关）
	var hardTitles []string
	var hardIDs []string
	for _, def := range sop.Definitions() {
		if def.Item.IsHardRule {
			hardIDs = append(hardIDs, def.Item.ID)
			hardTitles = append(hardTitles, def.Variants[0].Title)
		}
	}
	hardCtx := newTestSOPService(t, hardIDs, sop.PlanModeStandard).Rules(ctx, sop.Context{SessionID: "s"})
	if len(hardCtx) != len(all) {
		t.Fatalf("硬规则被关闭（%d → %d），安全边界不可关闭", len(all), len(hardCtx))
	}
	for _, title := range hardTitles {
		if !hasTitle(hardCtx, title) {
			t.Fatalf("硬规则「%s」不应被关闭", title)
		}
	}
}

// TestSOPServiceSubScope 子会话视角：main_only 消失、sub_only 出现。
func TestSOPServiceSubScope(t *testing.T) {
	svc := newTestSOPService(t, nil, sop.PlanModeStandard)
	ctx := context.Background()
	main := strings.Join(svc.Rules(ctx, sop.Context{SessionID: "s", IsSub: false}), "\n")
	sub := strings.Join(svc.Rules(ctx, sop.Context{SessionID: "s", IsSub: true}), "\n")

	for _, def := range sop.Definitions() {
		title := "【" + def.Variants[0].Title + "】"
		switch def.Item.Scope {
		case sop.ScopeMainOnly:
			if !strings.Contains(main, title) {
				t.Fatalf("main_only 规则 %s 未进主会话", def.Item.ID)
			}
			if strings.Contains(sub, title) {
				t.Fatalf("main_only 规则 %s 泄漏进子会话", def.Item.ID)
			}
		case sop.ScopeSubOnly:
			if !strings.Contains(sub, title) {
				t.Fatalf("sub_only 规则 %s 未进子会话", def.Item.ID)
			}
			if strings.Contains(main, title) {
				t.Fatalf("sub_only 规则 %s 泄漏进主会话", def.Item.ID)
			}
		}
	}
}

// TestSOPServicePlanModeFromProvider 未显式传 PlanMode 时用注入源（settings）的值。
func TestSOPServicePlanModeFromProvider(t *testing.T) {
	ctx := context.Background()
	svc := newTestSOPService(t, nil, sop.PlanModeAlwaysPlan)
	// 显式传空 PlanMode → 应读注入源 → 渲染 always_plan 变体
	joined := strings.Join(svc.Rules(ctx, sop.Context{SessionID: "s"}), "\n")
	if !strings.Contains(joined, "Always Plan 模式") {
		t.Fatal("未显式指定模式时应采用注入源（always_plan）")
	}
	// 显式传值优先于注入源（子 Run 强制 always_proceed 的场景依赖这一条）
	joined = strings.Join(svc.Rules(ctx, sop.Context{SessionID: "s", PlanMode: sop.PlanModeAlwaysProceed}), "\n")
	if !strings.Contains(joined, "Always Proceed 模式") {
		t.Fatal("显式 PlanMode 应覆盖注入源")
	}
}

// TestSOPServiceEffectiveConsistency 生效预览的 Rules 与 ActiveIDs 必须同口径。
//
// 回归：曾出现 Rules 已按关闭清单过滤、ActiveIDs 却仍按「全开」计算，
// 界面因此显示「已关闭但仍在生效」，用户无从判断开关是否真的生效。
func TestSOPServiceEffectiveConsistency(t *testing.T) {
	ctx := context.Background()
	var softID string
	for _, def := range sop.Definitions() {
		if !def.Item.IsHardRule && def.Item.Scope == sop.ScopeUniversal {
			softID = def.Item.ID
			break
		}
	}
	svc := newTestSOPService(t, []string{softID}, sop.PlanModeStandard)
	// base 传空串 → 预览以 core 正文开头（与真实装配的最小口径一致）
	view := svc.Effective(ctx, "", "", sop.Context{SessionID: "s"})

	if len(view.Rules) != len(view.ActiveIDs) {
		t.Fatalf("Rules 与 ActiveIDs 条数不一致（%d vs %d）", len(view.Rules), len(view.ActiveIDs))
	}
	for _, id := range view.ActiveIDs {
		if id == softID {
			t.Fatalf("已关闭规则 %s 仍被列为生效", softID)
		}
	}
	if !strings.Contains(view.SystemPreview, "## 工作规则") {
		t.Fatal("系统提示预览应包含规则段")
	}
	if !strings.Contains(view.SystemPreview, strings.TrimSpace(view.Core.Content)) {
		t.Fatal("系统提示预览应包含核心 SOP 正文档")
	}
	if view.TotalTokens < view.Core.Tokens {
		t.Fatal("总 token 应不小于核心 SOP 部分")
	}
}

// TestSOPServiceNilSafe 未装配 / 未注入时的降级行为（绝不 panic、绝不注入空提示）。
func TestSOPServiceNilSafe(t *testing.T) {
	var nilSvc *SOPService
	if got := nilSvc.Rules(context.Background(), sop.Context{}); len(got) != 0 {
		t.Fatalf("nil 服务应返回空规则，实际 %d 条", len(got))
	}
	// 未注入 disabledProvider / planModeProvider：按「全开 + standard」处理
	bare := NewSOPService(t.TempDir(), 800)
	rules := bare.Rules(context.Background(), sop.Context{SessionID: "s"})
	if len(rules) == 0 {
		t.Fatal("未注入开关源时应按全开渲染")
	}
	joined := strings.Join(rules, "\n")
	if !strings.Contains(joined, "方案先行与任务计划中枢治理规范") {
		t.Fatal("未注入规划模式时应回落 standard")
	}
}

// TestSOPServiceCatalog 目录必须可读且字段齐备。
func TestSOPServiceCatalog(t *testing.T) {
	items := newTestSOPService(t, nil, sop.PlanModeStandard).Catalog()
	if len(items) < 10 {
		t.Fatalf("目录应至少 10 条，实际 %d", len(items))
	}
	for _, it := range items {
		if it.ID == "" || it.Name == "" || it.Description == "" || it.DisableEffect == "" {
			t.Fatalf("目录字段缺失: %+v", it)
		}
		if len(it.Variants) == 0 {
			t.Fatalf("%s 缺少 variants", it.ID)
		}
	}
}
