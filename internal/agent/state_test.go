package agent

import (
	"testing"

	"harness-go/internal/model"
)

// 状态迁移矩阵：合法路径必须全部通过。
func TestCanTransitionLegalPaths(t *testing.T) {
	legal := [][2]model.RunStatus{
		{model.RunPending, model.RunRunning},
		{model.RunPending, model.RunCancelled},
		{model.RunPending, model.RunFailed},
		{model.RunPending, model.RunInterrupted},
		{model.RunRunning, model.RunWaitingApproval},
		{model.RunRunning, model.RunCompleted},
		{model.RunRunning, model.RunFailed},
		{model.RunRunning, model.RunCancelled},
		{model.RunRunning, model.RunInterrupted},
		{model.RunWaitingApproval, model.RunRunning},
		{model.RunWaitingApproval, model.RunFailed},
		{model.RunWaitingApproval, model.RunCancelled},
		{model.RunWaitingApproval, model.RunInterrupted},
	}
	for _, p := range legal {
		if !CanTransition(p[0], p[1]) {
			t.Fatalf("合法迁移被拒绝: %s → %s", p[0], p[1])
		}
		if err := CheckTransition(p[0], p[1]); err != nil {
			t.Fatalf("CheckTransition 应通过: %v", err)
		}
	}
}

// 终态不可再迁移（状态机单点收口的核心保证）。
func TestTerminalStatesAreFinal(t *testing.T) {
	terminals := []model.RunStatus{
		model.RunCompleted, model.RunFailed, model.RunCancelled, model.RunInterrupted,
	}
	all := []model.RunStatus{
		model.RunPending, model.RunRunning, model.RunWaitingApproval,
		model.RunCompleted, model.RunFailed, model.RunCancelled, model.RunInterrupted,
	}
	for _, from := range terminals {
		for _, to := range all {
			if CanTransition(from, to) {
				t.Fatalf("终态 %s 不应能迁移到 %s", from, to)
			}
		}
	}
}

// 跳跃迁移必须被拒绝。
func TestIllegalTransitions(t *testing.T) {
	illegal := [][2]model.RunStatus{
		{model.RunPending, model.RunCompleted},       // 不能跳过 running
		{model.RunPending, model.RunWaitingApproval}, // 审批只能从 running 进入
		{model.RunWaitingApproval, model.RunCompleted},
	}
	for _, p := range illegal {
		if CanTransition(p[0], p[1]) {
			t.Fatalf("非法迁移被放行: %s → %s", p[0], p[1])
		}
		if err := CheckTransition(p[0], p[1]); err == nil {
			t.Fatalf("CheckTransition 应报错: %s → %s", p[0], p[1])
		}
	}
}

func TestCheckTransitionRejectsUnknownStatus(t *testing.T) {
	if err := CheckTransition(model.RunPending, model.RunStatus("bogus")); err == nil {
		t.Fatal("非法目标状态应报错")
	}
}

func TestStatusHelpers(t *testing.T) {
	if !model.RunCompleted.IsTerminal() || model.RunRunning.IsTerminal() {
		t.Fatal("IsTerminal 判定错误")
	}
	if !model.RunWaitingApproval.IsUnfinished() || model.RunCompleted.IsUnfinished() {
		t.Fatal("IsUnfinished 判定错误")
	}
}
