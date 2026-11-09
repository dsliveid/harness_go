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
// 背景：移动端工作空间与纯对话会话不允许执行命令级工具。此时若仍走审批路径，
// 会滞留 waiting_approval 直到 DefaultTimeout=5min 超时才视为拒绝——用户白等 5 分钟。
// 因此必须在 Gate.Evaluate 之前短路，且 Run 必须继续完成（拒绝粒度是工具调用级）。
//
// 本文件把「拒绝 + Run 继续 + 不进入审批 + 工具未被真正执行」锁死为契约。

// DenyTools=true 且工具为 danger 级 → 立即拒绝，Run 继续完成，工具未被执行，
// 且绝不发出 approval.request（证明没有进入审批链路）。
func TestDenyToolsShortCircuitsCommandTool(t *testing.T) {
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
	req.DenyTools = true
	req.DenyReason = "当前为移动端，仅支持对话功能"

	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("拒绝工具后 Run 应正常完成，实际: %v", err)
	}

	// ① 工具绝不能被执行（短路发生在执行之前）。
	if tools.calls != 0 {
		t.Fatalf("仅对话模式不应执行命令级工具，实际执行 %d 次", tools.calls)
	}

	// ② 绝不能产生 approval.request 事件（说明没有进入审批链路）。
	for {
		select {
		case ev, ok := <-sub.C():
			if !ok {
				return // 通道关闭：全部事件已消费，且未见 approval.request
			}
			if ev.Type == events.TypeApprovalRequest {
				t.Fatal("仅对话模式不应发出 approval.request（必须短路，不得进审批）")
			}
		default:
			// 已排空，且未出现 approval.request。
			return
		}
	}
}

// DenyTools=true 但工具是只读级 → 不受影响，正常执行。
func TestDenyToolsAllowsReadonlyTool(t *testing.T) {
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
	req.DenyTools = true

	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("只读工具在仅对话模式下应可执行，实际: %v", err)
	}
	if tools.calls != 1 {
		t.Fatalf("只读工具应被执行 1 次，实际 %d 次", tools.calls)
	}
}

// DenyTools=false → 高危工具仍走既有审批链路（回归保护）。
func TestDenyToolsFalseKeepsApprovalPath(t *testing.T) {
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
	req.DenyTools = false

	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("审批超时拒绝后 Run 应继续完成，实际: %v", err)
	}
	if tools.calls != 0 {
		t.Fatalf("审批未放行时工具不应执行，实际 %d 次", tools.calls)
	}
}
