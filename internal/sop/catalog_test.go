package sop

import (
	"strings"
	"testing"
)

// mainCtx 主会话上下文。subCtx 子会话上下文。
func mainCtx() Context { return Context{SessionID: "sess01", IsSub: false} }
func subCtx() Context  { return Context{SessionID: "sess01", IsSub: true} }

func render(ctx Context) string {
	rules, _ := RenderRules(ctx, 1)
	return strings.Join(rules, "\n")
}

// TestDefinitionsNonEmpty 目录不得为空，且每条至少一个变体（否则是死项）。
func TestDefinitionsNonEmpty(t *testing.T) {
	defs := Definitions()
	if len(defs) < 10 {
		t.Fatalf("原生 SOP 目录应至少 10 条（对齐老版本 11 条），实际 %d", len(defs))
	}
	for _, d := range defs {
		if len(d.Variants) == 0 {
			t.Fatalf("%s 缺少 variants", d.Item.ID)
		}
		if strings.TrimSpace(d.Item.Description) == "" {
			t.Fatalf("%s 缺少 description（前端说明不可为空）", d.Item.ID)
		}
		if strings.TrimSpace(d.Item.DisableEffect) == "" {
			t.Fatalf("%s 缺少 disable_effect（关闭后果必须如实告知）", d.Item.ID)
		}
	}
}

// TestCatalogContract id 唯一、name 由 variants[0].title 派生、变体不含序号前缀。
func TestCatalogContract(t *testing.T) {
	items := Catalog()
	seen := map[string]bool{}
	for _, it := range items {
		if seen[it.ID] {
			t.Fatalf("SOP id 重复: %s", it.ID)
		}
		seen[it.ID] = true
		if it.Name != it.Variants[0].Title {
			t.Fatalf("%s 的 name 未从 variants[0].title 派生: %q vs %q",
				it.ID, it.Name, it.Variants[0].Title)
		}
		for _, v := range it.Variants {
			if v.Text != "" && v.Text[0] >= '0' && v.Text[0] <= '9' {
				t.Fatalf("%s 的变体 text 不应以序号开头", it.ID)
			}
		}
	}
}

// TestPromptKeyAnchors 关键标题锚点必须出现在真实渲染结果中（防改名/丢失）。
func TestPromptKeyAnchors(t *testing.T) {
	mainText := render(mainCtx())
	for _, anchor := range []string{
		"【Todo 先行与阶梯门禁公理（所有交互必须严格遵守）】",
		"【方案先行与任务计划中枢治理规范（防需求失真）】",
		"【任务清单全生命周期 SOP】",
		"【团队协同委派原则、弹性多并发与职责拆分准则】",
		"【边读边记与强制沉淀 SOP】",
		"【精益代码研读与规范探索 SOP】",
		"【代码克制与精确修改规范】",
		"【进程生命周期与破坏性操作安全红线（严防自杀与误杀）】",
	} {
		if !strings.Contains(mainText, anchor) {
			t.Fatalf("主会话提示词缺少锚点: %s", anchor)
		}
	}

	subText := render(subCtx())
	for _, anchor := range []string{
		"【纯黑板交付规范与结案硬性标准】",
		"【施工工人准则与架构遵从（必须严格遵守）】",
	} {
		if !strings.Contains(subText, anchor) {
			t.Fatalf("子会话提示词缺少锚点: %s", anchor)
		}
	}
}

// TestReportFilePlaceholder {report_file} 必须被替换，且不得残留原文。
func TestReportFilePlaceholder(t *testing.T) {
	ctx := subCtx()
	text := render(ctx)
	if strings.Contains(text, "{report_file}") {
		t.Fatal("占位符 {report_file} 残留原文")
	}
	if !strings.Contains(text, "sess01_report.md") {
		t.Fatal("占位符未被替换为 <sessionID>_report.md")
	}

	// SessionID 为空时用 subtask 兜底，绝不把字面量喂给模型。
	empty := render(Context{IsSub: true})
	if strings.Contains(empty, "{report_file}") {
		t.Fatal("SessionID 为空时仍残留占位符")
	}
	if !strings.Contains(empty, "subtask_report.md") {
		t.Fatal("SessionID 为空时应回落 subtask_report.md")
	}
}

// TestScopeIsolation main_only 绝不进子会话，sub_only 绝不进主会话。
func TestScopeIsolation(t *testing.T) {
	mainText := render(mainCtx())
	subText := render(subCtx())
	for _, def := range Definitions() {
		title := def.Variants[0].Title
		switch def.Item.Scope {
		case ScopeMainOnly:
			if !strings.Contains(mainText, title) {
				t.Fatalf("main_only 规则 %s 未出现在主会话提示词", def.Item.ID)
			}
			if strings.Contains(subText, title) {
				t.Fatalf("main_only 规则 %s 泄漏进子会话提示词", def.Item.ID)
			}
		case ScopeSubOnly:
			if !strings.Contains(subText, title) {
				t.Fatalf("sub_only 规则 %s 未出现在子会话提示词", def.Item.ID)
			}
			if strings.Contains(mainText, title) {
				t.Fatalf("sub_only 规则 %s 泄漏进主会话提示词", def.Item.ID)
			}
		}
	}
}

// TestEveryVariantReachable 无死项：每个变体至少在一种上下文中生效。
func TestEveryVariantReachable(t *testing.T) {
	ctxs := []Context{
		{SessionID: "s", IsSub: false, PlanMode: PlanModeStandard},
		{SessionID: "s", IsSub: false, PlanMode: PlanModeAlwaysPlan},
		{SessionID: "s", IsSub: false, PlanMode: PlanModeAlwaysProceed},
		{SessionID: "s", IsSub: true, PlanMode: PlanModeStandard},
		{SessionID: "s", IsSub: true, IsCollaborator: true, PlanMode: PlanModeStandard},
	}
	for _, def := range Definitions() {
		for i, v := range def.Variants {
			ok := false
			for _, c := range ctxs {
				if v.Active != nil && v.Active(c) {
					ok = true
					break
				}
			}
			if !ok {
				t.Fatalf("%s 的 variant#%d「%s」无任何生效场景（死项）", def.Item.ID, i, v.Label)
			}
		}
	}
}

// TestVariantMutualExclusion 同一条规则在一次渲染中只出现一次（变体互斥）。
func TestVariantMutualExclusion(t *testing.T) {
	for _, ctx := range []Context{
		{SessionID: "s", PlanMode: PlanModeStandard},
		{SessionID: "s", PlanMode: PlanModeAlwaysPlan},
		{SessionID: "s", PlanMode: PlanModeAlwaysProceed},
		{SessionID: "s", IsSub: true, PlanMode: PlanModeStandard},
	} {
		rules, _ := RenderRules(ctx, 1)
		for _, def := range Definitions() {
			for _, v := range def.Variants {
				c := 0
				for _, line := range rules {
					if strings.Contains(line, "【"+v.Title+"】") {
						c++
					}
				}
				if c > 1 {
					t.Fatalf("%s「%s」在同一渲染中出现 %d 次（应变体互斥）", def.Item.ID, v.Title, c)
				}
			}
		}
	}
}

// TestDisablingNonHardRuleRemovesExactlyOne 开关语义：非硬规则关闭后恰好少一条。
func TestDisablingNonHardRuleRemovesExactlyOne(t *testing.T) {
	for _, def := range Definitions() {
		ctx := mainCtx()
		if def.Item.Scope == ScopeSubOnly {
			ctx = subCtx()
		}
		all, _ := RenderRules(ctx, 1)
		ctx.Disabled = []string{def.Item.ID}
		filtered, _ := RenderRules(ctx, 1)

		if def.Item.IsHardRule {
			if len(filtered) != len(all) {
				t.Fatalf("%s 是硬规则，关闭不应生效（%d → %d）", def.Item.ID, len(all), len(filtered))
			}
			continue
		}
		// 只有「在该上下文中真实生效」的规则关闭后才会减少一条。
		active := false
		for _, v := range def.Variants {
			if v.Active != nil && v.Active(Context{SessionID: "sess01", IsSub: ctx.IsSub}) {
				active = true
				break
			}
		}
		if !active {
			continue
		}
		if len(filtered)+1 != len(all) {
			t.Fatalf("%s 关闭后应恰好减少 1 条（%d → %d）", def.Item.ID, len(all), len(filtered))
		}
	}
}

// TestPlanModeVariants 三种规划模式渲染互斥的措辞。
func TestPlanModeVariants(t *testing.T) {
	plan := render(Context{SessionID: "s", PlanMode: PlanModeAlwaysPlan})
	if !strings.Contains(plan, "【严格规划先行与确认门禁准则（Always Plan 模式）】") {
		t.Fatal("always_plan 应渲染确认门禁变体")
	}
	if strings.Contains(plan, "Always Proceed 模式") || strings.Contains(plan, "方案先行与任务计划中枢治理规范") {
		t.Fatal("always_plan 不应渲染其他模式变体")
	}

	proceed := render(Context{SessionID: "s", PlanMode: PlanModeAlwaysProceed})
	if !strings.Contains(proceed, "【规划先行与自动推进准则（Always Proceed 模式）】") {
		t.Fatal("always_proceed 应渲染自动推进变体")
	}
	if strings.Contains(proceed, "Always Plan 模式") {
		t.Fatal("always_proceed 不应渲染 always_plan 变体")
	}

	std := render(Context{SessionID: "s", PlanMode: PlanModeStandard})
	if !strings.Contains(std, "【方案先行与任务计划中枢治理规范（防需求失真）】") {
		t.Fatal("standard 应渲染标准变体")
	}
	if strings.Contains(std, "Always Plan 模式") || strings.Contains(std, "Always Proceed 模式") {
		t.Fatal("standard 不应渲染模式专属变体")
	}
}

// TestNumberingContinuous 编号必须连续（调用方给了起点后不跳号）。
func TestNumberingContinuous(t *testing.T) {
	rules, next := RenderRules(mainCtx(), 5)
	if len(rules) == 0 {
		t.Fatal("主会话应至少渲染一条规则")
	}
	for i, line := range rules {
		prefix := itoa(5+i) + ". "
		if !strings.HasPrefix(line, prefix) {
			t.Fatalf("第 %d 条编号应为 %d，实际: %s", i, 5+i, line)
		}
	}
	if next != 5+len(rules) {
		t.Fatalf("下一条可用编号应为 %d，实际 %d", 5+len(rules), next)
	}
}

// itoa 小工具（避免引入 strconv 只为拼一个测试断言）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestActiveIDsMatchesRender 生效 ID 与渲染结果条数一致（预览视图与真实注入同口径）。
func TestActiveIDsMatchesRender(t *testing.T) {
	ctx := Context{SessionID: "s", PlanMode: PlanModeStandard}
	rules, _ := RenderRules(ctx, 1)
	ids := ActiveIDs(ctx)
	if len(ids) != len(rules) {
		t.Fatalf("生效 ID 数 %d 与渲染条数 %d 不一致", len(ids), len(rules))
	}

	ctx.Disabled = []string{"safe_code_edit"}
	filteredRules, _ := RenderRules(ctx, 1)
	filteredIDs := ActiveIDs(ctx)
	if len(filteredIDs) != len(filteredRules) {
		t.Fatalf("关闭一条后仍不一致: ids=%d rules=%d", len(filteredIDs), len(filteredRules))
	}
	for _, id := range filteredIDs {
		if id == "safe_code_edit" {
			t.Fatal("已关闭的规则不应出现在生效 ID 中")
		}
	}
}

// TestHardRuleCannotBeDisabled 硬规则即使出现在 Disabled 中也不得消失。
func TestHardRuleCannotBeDisabled(t *testing.T) {
	var hardIDs []string
	for _, def := range Definitions() {
		if def.Item.IsHardRule {
			hardIDs = append(hardIDs, def.Item.ID)
		}
	}
	if len(hardIDs) == 0 {
		t.Fatal("目录应至少有一条硬规则（视觉规范 / 进程安全红线）")
	}
	ctx := mainCtx()
	all, _ := RenderRules(ctx, 1)
	ctx.Disabled = hardIDs
	filtered, _ := RenderRules(ctx, 1)
	if len(filtered) != len(all) {
		t.Fatalf("硬规则被关闭（%d → %d），安全边界不可关闭", len(all), len(filtered))
	}
}

// TestUnknownPlanModeFallsBack 未知模式回落 standard（宽容解析，不强加门禁）。
func TestUnknownPlanModeFallsBack(t *testing.T) {
	text := render(Context{SessionID: "s", PlanMode: "garbage-mode"})
	if !strings.Contains(text, "【方案先行与任务计划中枢治理规范（防需求失真）】") {
		t.Fatal("未知规划模式应回落 standard")
	}
}
