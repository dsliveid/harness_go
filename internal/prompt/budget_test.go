package prompt

import "testing"

// 预算内不应触发任何降级。
func TestPlanDegradeWithinBudget(t *testing.T) {
	plan := PlanDegrade(BudgetParams{MaxTotal: 1000, Current: 800}, LevelNone)
	if plan.Level != LevelNone || plan.Exhausted {
		t.Fatalf("预算内不应降级: %+v", plan)
	}
}

// MaxTotal<=0 表示不限预算，必须直接跳过（不能把 0 当"零预算"）。
func TestPlanDegradeUnlimitedBudget(t *testing.T) {
	plan := PlanDegrade(BudgetParams{MaxTotal: 0, Current: 999999}, LevelNone)
	if plan.Level != LevelNone || plan.Exhausted {
		t.Fatalf("不限预算不应降级: %+v", plan)
	}
}

// 第①级：有记忆可省时优先省记忆。
func TestPlanDegradeDropMemoryFirst(t *testing.T) {
	plan := PlanDegrade(BudgetParams{
		MaxTotal: 100, Current: 200,
		HasMemory: true, TopK: 8, CoreCount: 10,
	}, LevelNone)
	if plan.Level != LevelDropMemory || !plan.DropMemory {
		t.Fatalf("应先采用第①级省略记忆: %+v", plan)
	}
	if plan.TopK != 0 {
		t.Fatalf("第①级不应同时改动 topK: %+v", plan)
	}
}

// 无记忆可省 ⇒ 跳到第②级 topK 减半。
func TestPlanDegradeHalveTopK(t *testing.T) {
	plan := PlanDegrade(BudgetParams{
		MaxTotal: 100, Current: 200,
		HasMemory: false, TopK: 8, MinTopK: 2,
	}, LevelNone)
	if plan.Level != LevelHalveTopK || plan.TopK != 4 {
		t.Fatalf("应跳到第②级且 topK 8→4: %+v", plan)
	}
}

// topK 减半不得低于 MinTopK（防砍到 0 让按需工具完全不可见）。
func TestPlanDegradeHalveRespectsFloor(t *testing.T) {
	plan := PlanDegrade(BudgetParams{
		MaxTotal: 100, Current: 200,
		TopK: 8, MinTopK: 6,
	}, LevelHalveTopK-1) // from=1，从第②级开始看
	if plan.Level != LevelHalveTopK || plan.TopK != 6 {
		t.Fatalf("减半后应被 MinTopK 兜到 6: %+v", plan)
	}
}

// 第③级：常驻工具降到 4，且只在超过 4 时才有动作。
func TestPlanDegradeTrimCoreTools(t *testing.T) {
	plan := PlanDegrade(BudgetParams{
		MaxTotal: 100, Current: 200, CoreCount: 10,
	}, LevelHalveTopK) // from=2，从第③级起
	if plan.Level != LevelTrimCoreTools || plan.CoreLimit != CoreToolsFloor {
		t.Fatalf("应降到 %d 条: %+v", CoreToolsFloor, plan)
	}

	// 已等于下限时无从可降 ⇒ 继续往后找
	plan2 := PlanDegrade(BudgetParams{
		MaxTotal: 100, Current: 200, CoreCount: 4, HasOldToolResults: true,
	}, LevelHalveTopK)
	if plan2.Level != LevelCollapseToolResults {
		t.Fatalf("等于下限应跳到第④级: %+v", plan2)
	}
}

// 第④级：旧工具结果转引用。
func TestPlanDegradeCollapseToolResults(t *testing.T) {
	plan := PlanDegrade(BudgetParams{
		MaxTotal: 100, Current: 200, HasOldToolResults: true,
	}, LevelTrimCoreTools)
	if plan.Level != LevelCollapseToolResults || !plan.CollapseToolResults {
		t.Fatalf("应进入第④级: %+v", plan)
	}
}

// 第⑤级：允许压缩时产出压缩信号。
func TestPlanDegradeForceCompress(t *testing.T) {
	plan := PlanDegrade(BudgetParams{
		MaxTotal: 100, Current: 200, AllowCompress: true,
	}, LevelCollapseToolResults)
	if plan.Level != LevelForceCompress || !plan.NeedCompress {
		t.Fatalf("应进入第⑤级: %+v", plan)
	}
}

// 五档全无可用动作 ⇒ Exhausted，上层必须报错（严禁静默截断）。
func TestPlanDegradeExhausted(t *testing.T) {
	plan := PlanDegrade(BudgetParams{
		MaxTotal: 1, Current: 99999,
		// 全部无降级空间
		HasMemory: false, TopK: 0, CoreCount: 0,
		HasOldToolResults: false, AllowCompress: false,
	}, LevelNone)
	if !plan.Exhausted {
		t.Fatalf("无任何降级空间时必须置 Exhausted: %+v", plan)
	}
}

// 从末档继续规划 ⇒ 立即 Exhausted（防重复采用同一档导致死循环）。
func TestPlanDegradeAlreadyAtLast(t *testing.T) {
	plan := PlanDegrade(BudgetParams{
		MaxTotal: 1, Current: 99999, AllowCompress: true,
	}, LevelForceCompress)
	if !plan.Exhausted {
		t.Fatalf("已在末档仍超预算必须 Exhausted: %+v", plan)
	}
}

// MinTopK 为 0（异常输入）时减半仍须 >=1，不能产出 0 条。
func TestHalveNeverZero(t *testing.T) {
	if got := halve(1, 0); got < 1 {
		t.Fatalf("减半结果不得为 0，得到 %d", got)
	}
	if got := halve(0, 0); got != 0 {
		t.Fatalf("输入 0 应保持 0，得到 %d", got)
	}
}

// 档位名称必须可读（日志与错误信息依赖它）。
func TestLevelName(t *testing.T) {
	cases := map[DegradeLevel]string{
		LevelNone:                "未降级",
		LevelDropMemory:          "省略记忆层",
		LevelHalveTopK:           "topK 减半",
		LevelTrimCoreTools:       "常驻工具降到 4",
		LevelCollapseToolResults: "旧工具结果转引用",
		LevelForceCompress:       "强制压缩",
	}
	for lv, want := range cases {
		if got := LevelName(lv); got != want {
			t.Fatalf("档位 %d 名称应为 %q，得到 %q", int(lv), want, got)
		}
	}
}
