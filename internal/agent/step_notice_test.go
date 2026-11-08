package agent

import (
	"strings"
	"testing"
)

// TestStepNotice 校验步数进度提示的生成规则（纯函数，不依赖 fake 依赖）。
//
// 需求口径（2026-11）：
//   - 每 10 步注入一次「当前步数 X/总步数 N」；
//   - 最后一步必须注入收敛指令（不能只是普通进度提示）；
//   - every<=0 或 maxSteps<=0 时完全不注入（保持“可关闭”语义）。
func TestStepNotice(t *testing.T) {
	const maxSteps = 100
	const every = 10

	t.Run("非整10倍步不注入", func(t *testing.T) {
		for _, step := range []int{0, 1, 5, 7, 8} {
			if got := stepNotice(step, maxSteps, every); got != "" {
				t.Fatalf("step=%d 不应注入提示，实际: %q", step, got)
			}
		}
	})

	t.Run("第10步注入且步数文案正确", func(t *testing.T) {
		// step 从 0 计 → 第 10 步是 step=9
		got := stepNotice(9, maxSteps, every)
		if got == "" {
			t.Fatal("第 10 步（step=9）应注入进度提示，实际为空")
		}
		if !strings.Contains(got, "当前步数 10/总步数 100") {
			t.Fatalf("进度文案不含「当前步数 10/总步数 100」: %q", got)
		}
		if !strings.Contains(got, "剩余 90 步") {
			t.Fatalf("进度文案不含剩余步数: %q", got)
		}
	})

	t.Run("第20步再次注入", func(t *testing.T) {
		got := stepNotice(19, maxSteps, every)
		if !strings.Contains(got, "当前步数 20/总步数 100") {
			t.Fatalf("第 20 步文案错误: %q", got)
		}
	})

	t.Run("最后一步注入收敛指令", func(t *testing.T) {
		// 第 100 步 = step=99，此时已无余量，必须是收敛指令
		got := stepNotice(maxSteps-1, maxSteps, every)
		if !strings.Contains(got, "当前步数 100/总步数 100") {
			t.Fatalf("最后一步文案错误: %q", got)
		}
		if !strings.Contains(got, "已到最后一步") {
			t.Fatalf("最后一步缺少收敛指令: %q", got)
		}
		// 最后一步不应再出现「剩余 N 步」这种普通进度措辞
		if strings.Contains(got, "剩余 0 步") {
			t.Fatalf("最后一步不该用普通进度措辞: %q", got)
		}
	})

	t.Run("最后一步即使不命中整数倍也注入", func(t *testing.T) {
		// maxSteps=95、every=10：第 95 步不是 10 的倍数，但仍是最后一步
		got := stepNotice(94, 95, 10)
		if got == "" {
			t.Fatal("最后一步必须注入，即使不命中 every 的整数倍")
		}
		if !strings.Contains(got, "当前步数 95/总步数 95") {
			t.Fatalf("最后一步步数文案错误: %q", got)
		}
	})

	t.Run("关闭语义", func(t *testing.T) {
		if got := stepNotice(9, maxSteps, 0); got != "" {
			t.Fatalf("every=0 应关闭提示，实际: %q", got)
		}
		if got := stepNotice(9, maxSteps, -1); got != "" {
			t.Fatalf("every<0 应关闭提示，实际: %q", got)
		}
		if got := stepNotice(9, 0, every); got != "" {
			t.Fatalf("maxSteps<=0 应不注入，实际: %q", got)
		}
	})
}
