package agent

import (
	"fmt"

	"harness-go/internal/model"
)

// transitions Run 状态迁移矩阵（**状态机单点收口**，全项目只在这里定义）。
//
//	pending ──→ running ──→ completed
//	   │           ├──→ failed
//	   │           ├──→ cancelled
//	   │           ├──→ interrupted
//	   │           └──→ waiting_approval ──→ running（继续）
//	   │                                  ├──→ failed / cancelled / interrupted
//	   └──→ cancelled / failed / interrupted
//
// 终态（completed/failed/cancelled/interrupted）不可再迁移。
var transitions = map[model.RunStatus][]model.RunStatus{
	model.RunPending: {
		model.RunRunning,
		model.RunCancelled,
		model.RunFailed,
		model.RunInterrupted,
	},
	model.RunRunning: {
		model.RunWaitingApproval,
		model.RunCompleted,
		model.RunFailed,
		model.RunCancelled,
		model.RunInterrupted,
	},
	model.RunWaitingApproval: {
		model.RunRunning,
		model.RunFailed,
		model.RunCancelled,
		model.RunInterrupted,
	},
	model.RunCompleted:   {},
	model.RunFailed:      {},
	model.RunCancelled:   {},
	model.RunInterrupted: {},
}

// CanTransition 判定状态迁移是否合法。
func CanTransition(from, to model.RunStatus) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// CheckTransition 非法迁移返回错误（便于调用方直接返回）。
func CheckTransition(from, to model.RunStatus) error {
	if !to.Valid() {
		return fmt.Errorf("agent: 非法目标状态 %q", to)
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("agent: 非法状态迁移 %s → %s", from, to)
	}
	return nil
}
