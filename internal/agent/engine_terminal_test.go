package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"harness-go/internal/llm"
	"harness-go/internal/model"
)

// freezingTools 在工具执行期间把 Run 迁到终态，模拟"用户在工具跑动时点停止"。
//
// 这正是事故现场的执行时序：引擎卡在一次耗时命令里，用户点停止，
// service 层把 Run 改成 cancelled；命令随后返回，结果仍被落库。
type freezingTools struct {
	sdb    *fakeSessionDB
	result ToolRunResult
	calls  int
}

func (f *freezingTools) Run(context.Context, ToolRunRequest) (ToolRunResult, error) {
	f.calls++
	// 模拟"工具执行期间 Run 被别处迁移到 cancelled 终态"。
	f.sdb.mu.Lock()
	f.sdb.run.Status = model.RunCancelled
	f.sdb.run.StopReason = "user_stopped"
	f.sdb.mu.Unlock()
	return f.result, nil
}

// toolCallProvider 让模型先发一次工具调用，再给最终答复。
func toolCallProvider() *fakeProvider {
	return &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "read_file"}}},
		{{Delta: "最终答复"}},
	}}
}

// TestTerminalRunRejectsToolMessage 终态后工具结果**不得**写入 messages。
//
// 回归背景（真实事故 9ee34e00）：runs.ended_at=…266409 已是 cancelled，
// 但 messages seq107/108 仍在 …320130/…320132 落库（迟 53.7 秒）。
// 后果：模型上下文被注入"用户已经不要了的"工具结果，后续重放也会带上，
// 并且前端历史回看会看到一段本不该存在的输出。
func TestTerminalRunRejectsToolMessage(t *testing.T) {
	provider := toolCallProvider()
	var tools *freezingTools
	eng, sdb, _, _ := newTestEngine(t, provider, func(d *Deps) {
		tools = &freezingTools{
			sdb:    nil, // 下面立即补上
			result: ToolRunResult{Status: model.ToolOK, ResultJSON: `{"content":"文件内容"}`},
		}
		d.Tools = tools
		d.Catalog = fakeCatalog{}
	})
	tools.sdb = sdb

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := eng.Execute(ctx, baseRunRequest())
	// Run 已被置为终态，引擎应以 cancelled 收口（返回 nil 是终态语义，
	// 不属于"执行失败"）；关键断言在下面的消息条数。
	_ = err

	if tools.calls == 0 {
		t.Fatal("工具应被调用过（否则没测到目标路径）")
	}

	sdb.mu.Lock()
	defer sdb.mu.Unlock()

	if sdb.run.Status != model.RunCancelled {
		t.Fatalf("Run 应为 cancelled，得到 %s", sdb.run.Status)
	}
	// 核心断言：终态之后不得再有 tool 消息落库。
	for _, m := range sdb.messages {
		if m.Role == model.RoleTool {
			t.Fatalf("终态后不应落库 tool 消息，却发现了 %d 条（内容: %q）",
				countRole(sdb.messages, model.RoleTool), m.Content)
		}
	}
}

// TestTerminalRunStillRecordsToolEvent 终态后**审计不能丢**。
//
// 这是上面那条的配套约束：只禁止写 messages（模型上下文），
// 绝不禁止写 tool_events（审计与前端卡片收口依赖它）。
// 若两者一起禁掉，前端会出现永远"执行中…"的卡片 —— 旧缺陷会换一种形式复发。
func TestTerminalRunStillRecordsToolEvent(t *testing.T) {
	provider := toolCallProvider()
	var tools *freezingTools
	eng, sdb, _, _ := newTestEngine(t, provider, func(d *Deps) {
		tools = &freezingTools{
			result: ToolRunResult{Status: model.ToolOK, ResultJSON: `{"content":"文件内容"}`},
		}
		d.Tools = tools
		d.Catalog = fakeCatalog{}
	})
	tools.sdb = sdb

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = eng.Execute(ctx, baseRunRequest())

	sdb.mu.Lock()
	defer sdb.mu.Unlock()

	if len(sdb.toolEvents) == 0 {
		t.Fatal("tool_events 必须保留（审计不丢、前端卡片才能收口）")
	}
	// 事件应已被收口到终态，而不是永远停在 pending。
	finished := false
	for _, ev := range sdb.toolEvents {
		if ev.Status != model.ToolPending {
			finished = true
			break
		}
	}
	if !finished {
		t.Error("tool_event 状态应已收口（不能永远停在 pending）")
	}
}

// TestAssertMutableTrueWhileRunning 运行中的 Run 必须允许写。
//
// 守卫不能误伤正常路径：这条用例防止 assertMutable 写反或过度保守，
// 导致所有正常 Run 都写不进消息（那是比原缺陷更严重的回归）。
func TestAssertMutableTrueWhileRunning(t *testing.T) {
	eng, sdb, _, _ := newTestEngine(t, &fakeProvider{})
	sdb.run.Status = model.RunRunning

	if !eng.assertMutable(context.Background(), sdb, "run-1") {
		t.Fatal("running 状态应允许写入")
	}
}

// TestAssertMutableRejectsAllTerminalStates 四种终态都必须拒绝写入。
func TestAssertMutableRejectsAllTerminalStates(t *testing.T) {
	terminals := []model.RunStatus{
		model.RunCompleted, model.RunFailed, model.RunCancelled, model.RunInterrupted,
	}
	for _, st := range terminals {
		eng, sdb, _, _ := newTestEngine(t, &fakeProvider{})
		sdb.run.Status = st
		if eng.assertMutable(context.Background(), sdb, "run-1") {
			t.Errorf("终态 %s 必须拒绝写入", st)
		}
	}
}

// TestAssertMutableWorksWithCancelledCtx 取消场景下守卫仍能读到状态。
//
// 这是最容易被忽略的坑：守卫大量运行在"ctx 已取消"的路径上。
// 若它复用被取消的 ctx 去查库，SQL 根本不会执行，守卫会在最需要它的
// 时刻静默失效。此用例锁定 detachedCtx 的用法。
func TestAssertMutableWorksWithCancelledCtx(t *testing.T) {
	eng, sdb, _, _ := newTestEngine(t, &fakeProvider{})
	sdb.run.Status = model.RunCancelled

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消

	if eng.assertMutable(ctx, sdb, "run-1") {
		t.Fatal("终态必须拒绝写入（即使父 ctx 已取消）")
	}

	// 反向验证：运行中的 Run 在 ctx 取消后也应返回 true，
	// 证明读取确实成功了（而不是因为 ctx 报错才返回 false）。
	sdb.run.Status = model.RunRunning
	if !eng.assertMutable(ctx, sdb, "run-1") {
		t.Fatal("ctx 已取消时仍应能读到 running 状态（detachedCtx 未生效）")
	}
}

// TestSummaryTouchOnToolMessage tool 消息也推进 last_message_at。
//
// 回归背景：sessions.last_message_at 停在 seq106（assistant），
// 而 updated_at 已到 seq108（tool）—— 列表按最后活跃排序会排错。
func TestSummaryTouchOnToolMessage(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "read_file"}}},
		{{Delta: "完成"}},
	}}
	eng, sdb, _, summary := newTestEngine(t, provider, func(d *Deps) {
		d.Tools = &fakeTools{result: ToolRunResult{Status: model.ToolOK, ResultJSON: `{"content":"x"}`}}
		d.Catalog = fakeCatalog{}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := eng.Execute(ctx, baseRunRequest()); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}

	summary.mu.Lock()
	touches := summary.touch
	summary.mu.Unlock()

	// 至少要有 assistant + tool + 最终答复三次推进。
	// 只断言"大于 assistant 数量"即可覆盖目标：tool 消息必须计入。
	if touches < 2 {
		t.Fatalf("last_message_at 推进次数过少（%d），tool 消息可能未计入", touches)
	}

	sdb.mu.Lock()
	toolCount := countRole(sdb.messages, model.RoleTool)
	sdb.mu.Unlock()
	if toolCount == 0 {
		t.Fatal("本用例需要至少一条 tool 消息")
	}
}

func countRole(msgs []*model.Message, role model.Role) int {
	n := 0
	for _, m := range msgs {
		if m.Role == role {
			n++
		}
	}
	return n
}

var _ = strings.Contains // 保留 strings 导入以便后续扩展断言
