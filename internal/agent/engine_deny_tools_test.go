package agent

import (
	"context"
	"testing"
	"time"

	"harness-go/internal/approval"
	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/model"
)

// ============ 能力天花板短路拒绝（协议 §4.1 / §4.2） ============
//
// 背景：工作空间能力天花板不允许某些工具执行。此时若仍走审批路径，
// 会滞留 waiting_approval 直到 DefaultTimeout=5min 超时才视为拒绝——用户白等 5 分钟。
// 因此必须在 Gate.Evaluate 之前短路，且 Run 必须继续完成（拒绝粒度是工具调用级）。
//
// 本文件把「拒绝 + Run 继续 + 不进入审批 + 工具未被真正执行」锁死为契约，
// 并锁死三态策略 ToolPolicy（full / readonly / none）的语义边界。
//
// 🚨 回归保护重点：旧的 DenyTools 布尔只拦 command + danger，write 级被放行。
// 下面 TestToolPolicyNoneBlocksWriteTool 就是那条缺陷的防倒退测试。

// policy=false 时按 full 处理（零值兼容）：只读工具与写工具都应正常执行。
func TestToolPolicyReadonlyAllowsReadonlyTool(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "read_file"}}},
		{{Delta: "读完了"}},
	}}
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK}}

	eng, _, _, _ := newTestEngine(t, provider, func(d *Deps) {
		d.Tools = tools
		d.Catalog = readOnlyCatalog{}
		d.Gate = approval.NewGate(30 * time.Second)
	})

	req := baseRunRequest()
	req.ToolPolicy = ToolPolicyReadonly

	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("只读工具在 readonly 策略下应可执行，实际: %v", err)
	}
	if tools.calls != 1 {
		t.Fatalf("只读工具应被执行 1 次，实际 %d 次", tools.calls)
	}
}

// readonly 策略 + command 级工具 → 立即拒绝，Run 继续，绝不进审批链路。
func TestToolPolicyReadonlyBlocksCommandTool(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "run_command"}}},
		{{Delta: "好的，我改为只聊天"}},
	}}
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK}}

	eng, _, bus, _ := newTestEngine(t, provider, func(d *Deps) {
		d.Tools = tools
		d.Catalog = dangerCatalog{}
		// 闸门足够长：若错误地走了审批路径，本测试会阻塞到超时而非静默通过。
		d.Gate = approval.NewGate(30 * time.Second)
	})

	// 提前订阅全部事件（Bus 是广播，事后订阅会漏已发事件）。
	sub := bus.SubscribeAll()
	defer bus.Unsubscribe(sub)

	req := baseRunRequest()
	req.ToolPolicy = ToolPolicyReadonly
	req.PolicyReason = "当前为移动端，仅支持只读操作与对话"

	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("拒绝工具后 Run 应正常完成，实际: %v", err)
	}

	// ① 工具绝不能被执行（短路发生在执行之前）。
	if tools.calls != 0 {
		t.Fatalf("readonly 策略不应执行 command 级工具，实际执行 %d 次", tools.calls)
	}

	// ② 绝不能产生 approval.request 事件（说明没有进入审批链路）。
	for {
		select {
		case ev, ok := <-sub.C():
			if !ok {
				return // 通道关闭：全部事件已消费，且未见 approval.request
			}
			if ev.Type == events.TypeApprovalRequest {
				t.Fatal("readonly 策略不应发出 approval.request（必须短路，不得进审批）")
			}
		default:
			// 已排空，且未出现 approval.request。
			return
		}
	}
}

// 🚨 防倒退核心：none 策略必须拦下 **write** 级工具。
//
// 这正是旧 DenyTools 布尔的缺陷所在（它只拦 command+danger），
// 若不修，「无设备空间零工具」与「移动端禁写文件」两条契约同时失效。
func TestToolPolicyNoneBlocksWriteTool(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "write_file"}}},
		{{Delta: "好的，我不再尝试写文件"}},
	}}
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK}}

	eng, _, _, _ := newTestEngine(t, provider, func(d *Deps) {
		d.Tools = tools
		d.Catalog = writeCatalog{} // write_file = write 级
		d.Gate = approval.NewGate(30 * time.Second)
	})

	req := baseRunRequest()
	req.ToolPolicy = ToolPolicyNone

	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("拒绝工具后 Run 应正常完成，实际: %v", err)
	}
	if tools.calls != 0 {
		t.Fatalf("none 策略必须拦下 write 级工具，实际执行 %d 次（旧 DenyTools 缺陷回归）", tools.calls)
	}
}

// none 策略 + 只读工具 → 同样拒绝（零工具是绝对语义，不是「少给点能力」）。
func TestToolPolicyNoneBlocksReadonlyTool(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "read_file"}}},
		{{Delta: "好的"}},
	}}
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK}}

	eng, _, _, _ := newTestEngine(t, provider, func(d *Deps) {
		d.Tools = tools
		d.Catalog = readOnlyCatalog{}
		d.Gate = approval.NewGate(30 * time.Second)
	})

	req := baseRunRequest()
	req.ToolPolicy = ToolPolicyNone

	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("拒绝工具后 Run 应正常完成，实际: %v", err)
	}
	if tools.calls != 0 {
		t.Fatalf("none 策略应拦下只读工具（零工具语义），实际执行 %d 次", tools.calls)
	}
}

// full 策略（默认零值）→ 高危工具仍走既有审批链路（回归保护）。
func TestToolPolicyFullKeepsApprovalPath(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "run_command"}}},
		{{Delta: "已放弃"}},
	}}
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK}}

	eng, _, _, _ := newTestEngine(t, provider, func(d *Deps) {
		d.Tools = tools
		d.Catalog = dangerCatalog{}
		// 短超时：审批超时视为拒绝，Run 继续（既有行为）。
		d.Gate = approval.NewGate(60 * time.Millisecond)
	})

	req := baseRunRequest()
	req.ToolPolicy = ToolPolicyFull

	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("审批超时拒绝后 Run 应继续完成，实际: %v", err)
	}
	if tools.calls != 0 {
		t.Fatalf("审批未放行时工具不应执行，实际 %d 次", tools.calls)
	}
}

// Allows 的三态语义表（纯函数，不依赖引擎）。
func TestToolPolicyAllows(t *testing.T) {
	cases := []struct {
		policy ToolPolicy
		risk   model.Risk
		want   bool
	}{
		{ToolPolicyFull, model.RiskReadonly, true},
		{ToolPolicyFull, model.RiskWrite, true},
		{ToolPolicyFull, model.RiskCommand, true},
		{ToolPolicyFull, model.RiskDanger, true},
		{ToolPolicyReadonly, model.RiskReadonly, true},
		{ToolPolicyReadonly, model.RiskWrite, false},
		{ToolPolicyReadonly, model.RiskCommand, false},
		{ToolPolicyReadonly, model.RiskDanger, false},
		{ToolPolicyNone, model.RiskReadonly, false},
		{ToolPolicyNone, model.RiskWrite, false},
		{ToolPolicyNone, model.RiskCommand, false},
		{ToolPolicyNone, model.RiskDanger, false},
		// 零值兼容：未接入策略的既有调用点按 full 处理，零回归。
		{"", model.RiskDanger, true},
		{"", model.RiskReadonly, true},
	}
	for _, c := range cases {
		if got := c.policy.Allows(c.risk); got != c.want {
			t.Errorf("policy=%q risk=%q: 期望 %v，实际 %v", c.policy, c.risk, c.want, got)
		}
	}
}
