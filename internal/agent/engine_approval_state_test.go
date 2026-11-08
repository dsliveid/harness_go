package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"harness-go/internal/approval"
	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/model"
)

// ============ 审批状态机闭环（单一真相铁律） ============
//
// 背景（实测事故）：引擎曾只发 approval.request 就阻塞在 Gate.Await，**从不**迁移
// runs.status=waiting_approval。连锁后果三处：
//   - ① DecideApproval 硬校验 run.Status==waiting_approval → 用户点「允许」永远 409；
//   - ② GET /active-run 的 status 永不为 waiting_approval → 前端「刷新后重建审批条」成死代码；
//   - ③ 跨端/审计无法区分「在跑」与「在等人」。
//
// 本文件把「审批期间必为 waiting_approval、决策后必回 running」锁死为契约。

// approvalProbe 在收到 approval.request 时采样 Run 状态，并可选地立刻提交决策。
type approvalProbe struct {
	mu         sync.Mutex
	statusSeen model.RunStatus
	payload    map[string]any
	decision   approval.Decision
	resolve    bool
}

func (p *approvalProbe) snapshot() (model.RunStatus, map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.statusSeen, p.payload
}

// watchApproval 订阅事件流，在 approval.request 到达时采样状态（此时闸门已阻塞）。
//
// 采样点语义：approval.request 是 publish 出来的，若引擎先 publish 后 transition，
// 这里就会采到 running —— 正是要防的回归，故断言点设在此处。
func watchApproval(t *testing.T, bus *events.Bus, sdb *fakeSessionDB, probe *approvalProbe, gate *approval.Gate) {
	t.Helper()
	sub, err := bus.Subscribe("run-1", 0)
	if err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	go func() {
		defer bus.Unsubscribe(sub)
		for ev := range sub.C() {
			if ev.Type != events.TypeApprovalRequest {
				continue
			}
			var payload map[string]any
			_ = json.Unmarshal(ev.Payload, &payload)
			run, err := sdb.GetRun(context.Background(), "run-1")
			probe.mu.Lock()
			if err == nil {
				probe.statusSeen = run.Status
			}
			probe.payload = payload
			wantResolve := probe.resolve
			decision := probe.decision
			probe.mu.Unlock()
			if wantResolve {
				if id, ok := payload["approval_id"].(string); ok {
					gate.Resolve(id, decision)
				}
			}
			return // 只处理第一条审批
		}
	}()
}

// 审批期间 Run 必为 waiting_approval；用户决策后回到 running 并最终 completed。
func TestEngineApprovalTransitionsWaitingAndBack(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "write_file"}}},
		{{Delta: "已写入"}},
	}}
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK}}
	gate := approval.NewGate(5 * time.Second)
	probe := &approvalProbe{decision: approval.AllowOnce, resolve: true}

	eng, sdb, bus, _ := newTestEngine(t, provider, func(d *Deps) {
		d.Tools = tools
		d.Gate = gate
	})
	watchApproval(t, bus, sdb, probe, gate)

	req := baseRunRequest()
	req.AccessMode = approval.ModeConfirm // write 工具需人工审批
	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("决策通过后 Run 应完成: %v", err)
	}

	seen, payload := probe.snapshot()
	if seen != model.RunWaitingApproval {
		t.Fatalf("审批期间 Run 状态应为 waiting_approval，实际采样到 %q（引擎未迁移状态）", seen)
	}
	if payload == nil {
		t.Fatal("应产生 approval_request 事件")
	}
	// 审批后状态必须回到 running 才允许继续执行工具；终态 completed 证明回迁成功。
	if sdb.run.Status != model.RunCompleted {
		t.Fatalf("最终应为 completed，得到 %s", sdb.run.Status)
	}
	if tools.calls != 1 {
		t.Fatalf("允许后工具应执行 1 次，得到 %d", tools.calls)
	}
}

// approval.request 必须携带 run_id 与 args：前端据此显示「放行哪个 Run 的什么参数」，
// 刷新恢复时也靠 run_id 决定决策提交目标（此前两者缺失，前端读出来恒为空）。
func TestApprovalRequestCarriesRunIDAndArgs(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "write_file",
			ArgumentsDelta: `{"path":"a.txt"}`}}},
		{{Delta: "已放弃"}},
	}}
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK}}
	// 短超时闸门：不提交决策，审批超时视为拒绝（不影响断言事件形状）
	gate := approval.NewGate(80 * time.Millisecond)
	probe := &approvalProbe{}

	eng, sdb, bus, _ := newTestEngine(t, provider, func(d *Deps) {
		d.Tools = tools
		d.Gate = gate
	})
	watchApproval(t, bus, sdb, probe, gate)

	req := baseRunRequest()
	req.AccessMode = approval.ModeConfirm
	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("超时拒绝后 Run 应继续完成: %v", err)
	}

	_, payload := probe.snapshot()
	if payload == nil {
		t.Fatal("应产生 approval_request 事件")
	}
	if got, _ := payload["run_id"].(string); got != "run-1" {
		t.Fatalf("approval_request.run_id 应为 run-1，得到 %v", payload["run_id"])
	}
	args, _ := payload["args"].(string)
	if args == "" {
		t.Fatal("approval_request.args 不应为空（前端审批条要展示待放行参数）")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(args), &decoded); err != nil {
		t.Fatalf("args 应为合法 JSON 字符串: %v", err)
	}
	if decoded["path"] != "a.txt" {
		t.Fatalf("args 应保留工具原始参数，得到 %v", decoded)
	}
}

// 审批超时（视为拒绝）后 Run 也必须从 waiting_approval 回到 running 再收口，
// 否则 waiting_approval 会成为悬挂态（重启恢复与列表徽标都会误判）。
func TestApprovalTimeoutLeavesNoDanglingWaitingState(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "write_file"}}},
		{{Delta: "换个方式"}},
	}}
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK}}
	gate := approval.NewGate(60 * time.Millisecond)
	probe := &approvalProbe{}

	eng, sdb, bus, _ := newTestEngine(t, provider, func(d *Deps) {
		d.Tools = tools
		d.Gate = gate
	})
	watchApproval(t, bus, sdb, probe, gate)

	req := baseRunRequest()
	req.AccessMode = approval.ModeConfirm
	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("超时拒绝后 Run 应继续完成: %v", err)
	}
	if seen, _ := probe.snapshot(); seen != model.RunWaitingApproval {
		t.Fatalf("审批等待期间应为 waiting_approval，实际 %q", seen)
	}
	if sdb.run.Status != model.RunCompleted {
		t.Fatalf("超时拒绝后应回到 running 并完成，实际 %s", sdb.run.Status)
	}
	if tools.calls != 0 {
		t.Fatalf("被拒工具不应执行，实际 %d 次", tools.calls)
	}
}
