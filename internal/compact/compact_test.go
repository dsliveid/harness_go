package compact

import (
	"strings"
	"testing"
	"unicode/utf8"

	"harness-go/internal/llm"
	"harness-go/internal/model"
)

// msg 构造一条消息的便捷函数。
func msg(seq int64, role model.Role, content string) model.Message {
	return model.Message{Seq: seq, Role: role, Content: content}
}

// toolCallMsg 构造带工具调用声明的 assistant 消息。
func toolCallMsg(seq int64) model.Message {
	return model.Message{
		Seq:           seq,
		Role:          model.RoleAssistant,
		ToolCallsJSON: []byte(`[{"ID":"call_1","Name":"read_file"}]`),
	}
}

// ---- Evaluate：水位算术 ----

// TestEvaluateThreshold 80% 水位线必须精确。
func TestEvaluateThreshold(t *testing.T) {
	cases := []struct {
		used, limit int
		needs       bool
	}{
		{0, 1000, false},
		{799, 1000, false},
		{800, 1000, true}, // 恰好 80% 必须触发（不是 801）
		{801, 1000, true},
		{1000, 1000, true},
	}
	for _, c := range cases {
		v := Evaluate(c.used, c.limit)
		if v.NeedsCompaction != c.needs {
			t.Errorf("used=%d limit=%d 期望 needs=%v，得到 %v（阈值 %d）",
				c.used, c.limit, c.needs, v.NeedsCompaction, v.Threshold)
		}
	}
}

// TestEvaluateUnknownLimit 窗口未知（0）时不得触发压缩。
//
// 理由：基于猜测的窗口做破坏性折叠，比让预算闸门明确报错更糟。
func TestEvaluateUnknownLimit(t *testing.T) {
	v := Evaluate(999999, 0)
	if v.NeedsCompaction {
		t.Error("limit<=0 时不得触发压缩")
	}
	if v.Threshold != 0 {
		t.Errorf("limit<=0 时阈值应为 0，得到 %d", v.Threshold)
	}
}

// TestEvaluatePercentage 百分比保留一位小数，不出现浮点噪声。
func TestEvaluatePercentage(t *testing.T) {
	v := Evaluate(1234, 10000)
	if v.Percentage != 12.3 {
		t.Errorf("百分比应为 12.3，得到 %v", v.Percentage)
	}
}

// ---- PlanCompaction：活跃轮次保护（本包最关键的语义）----

// TestPlanRunningKeepsActiveTurn 运行中必须保留当前轮次。
//
// 这是与"砍最旧 N 条"的根本区别：事故会话是「1 条 user + 47 轮工具」，
// 若按条数砍最旧，会把唯一的 user 消息砍掉——模型彻底失忆。
func TestPlanRunningKeepsActiveTurn(t *testing.T) {
	msgs := []model.Message{
		msg(1, model.RoleUser, "第一个任务"),
		msg(2, model.RoleAssistant, "好的"),
		msg(3, model.RoleTool, "工具结果"),
		msg(4, model.RoleUser, "现在做第二个任务"), // 当前轮起点
		msg(5, model.RoleAssistant, "正在做"),
		msg(6, model.RoleTool, "结果"),
	}

	p := PlanCompaction(msgs, 0, true)

	if p.Empty() {
		t.Fatal("应能折叠第一个已完成轮次")
	}
	if p.EndSeq != 3 {
		t.Errorf("应折叠到 seq 3（第二轮的 user 之前），得到 %d", p.EndSeq)
	}
	// 核心断言：当前轮起点（seq 4）绝不能被折叠。
	for _, m := range p.Messages {
		if m.Seq >= 4 {
			t.Fatalf("当前轮次的消息 seq=%d 被误折叠（会导致模型对当前任务失忆）", m.Seq)
		}
	}
}

// TestPlanRunningSingleTurnNoCompaction 单轮任务运行中不得压缩。
//
// 事故会话正是这个形态：1 条 user + 大量工具。运行期间压缩它
// 等于把任务本身删掉。
func TestPlanRunningSingleTurnNoCompaction(t *testing.T) {
	var msgs []model.Message
	msgs = append(msgs, msg(1, model.RoleUser, "分析启动变慢"))
	for i := int64(2); i <= 50; i++ {
		msgs = append(msgs, toolCallMsg(i))
		msgs = append(msgs, msg(i+1, model.RoleTool, "大量输出"))
		i++
	}

	p := PlanCompaction(msgs, 0, true)
	if !p.Empty() {
		t.Fatalf("单轮任务运行中不得压缩，却规划了 seq[%d,%d]", p.StartSeq, p.EndSeq)
	}
}

// TestPlanIdleCompactsAll 空闲态可折叠整段已完成历史。
func TestPlanIdleCompactsAll(t *testing.T) {
	msgs := []model.Message{
		msg(1, model.RoleUser, "任务一"),
		msg(2, model.RoleAssistant, "完成一"),
		msg(3, model.RoleUser, "任务二"),
		msg(4, model.RoleAssistant, "完成二"),
	}
	p := PlanCompaction(msgs, 0, false)
	if p.EndSeq != 4 {
		t.Errorf("空闲态应折叠全部 4 条，得到 end=%d", p.EndSeq)
	}
	if len(p.Messages) != 4 {
		t.Errorf("应包含 4 条消息，得到 %d", len(p.Messages))
	}
}

// TestPlanSkipsAlreadyCompacted 已压缩区间不得重复压缩。
func TestPlanSkipsAlreadyCompacted(t *testing.T) {
	msgs := []model.Message{
		msg(1, model.RoleUser, "老的"),
		msg(2, model.RoleAssistant, "老回复"),
		msg(3, model.RoleUser, "新的"),
		msg(4, model.RoleAssistant, "新回复"),
	}
	p := PlanCompaction(msgs, 2, false)
	if p.StartSeq != 3 {
		t.Errorf("应从 seq 3 开始，得到 %d", p.StartSeq)
	}
}

// TestPlanSkipsHidden 置灰（被回滚）的消息不参与压缩。
func TestPlanSkipsHidden(t *testing.T) {
	hidden := msg(2, model.RoleAssistant, "已回滚")
	hidden.Hidden = true
	msgs := []model.Message{
		msg(1, model.RoleUser, "keep"),
		hidden,
		msg(3, model.RoleAssistant, "keep2"),
	}
	p := PlanCompaction(msgs, 0, false)
	for _, m := range p.Messages {
		if m.Seq == 2 {
			t.Fatal("已置灰的消息不应被折叠进摘要")
		}
	}
}

// TestPlanAvoidsDanglingToolCalls 折叠区间末尾不得留下悬空的 tool_calls。
//
// 若把 assistant(tool_calls) 包进摘要、却把配对的 tool 结果留在外面，
// 重建历史时上游 provider 会因"工具调用无结果"直接 400。
func TestPlanAvoidsDanglingToolCalls(t *testing.T) {
	msgs := []model.Message{
		msg(1, model.RoleUser, "任务"),
		toolCallMsg(2), // 带 tool_calls，但没有配对结果
		msg(3, model.RoleUser, "新任务"),
		msg(4, model.RoleAssistant, "回复"),
	}

	p := PlanCompaction(msgs, 0, true)
	// 边界本应是 2（折叠 seq1-2），但 seq2 是悬空 tool_calls，
	// 必须收缩到 seq1 之前 → 无内容可折叠。
	if !p.Empty() {
		for _, m := range p.Messages {
			if m.Seq == 2 {
				t.Fatalf("悬空的 tool_calls 消息不应落在折叠区间内（区间 [%d,%d]）",
					p.StartSeq, p.EndSeq)
			}
		}
	}
}

// TestPlanBoundaryIncludesToolResult tool_calls 与其 tool 结果成对时正常折叠。
func TestPlanBoundaryIncludesToolResult(t *testing.T) {
	msgs := []model.Message{
		msg(1, model.RoleUser, "任务"),
		toolCallMsg(2),
		msg(3, model.RoleTool, "结果"),
		msg(4, model.RoleUser, "下一轮"),
	}
	p := PlanCompaction(msgs, 0, true)
	if p.Empty() {
		t.Fatal("应能折叠 seq1-3（含完整工具调用对）")
	}
	if p.EndSeq != 3 {
		t.Errorf("应折叠到 seq3，得到 %d", p.EndSeq)
	}
}

// ---- BuildSummaryPrompt ----

// TestSummaryPromptKeepsGoal 摘要提示词必须强制保留用户目标。
//
// 若丢了目标，模型后续会静默跑偏——用户只看到答非所问，完全不知原因。
func TestSummaryPromptKeepsGoal(t *testing.T) {
	prompt := BuildSummaryPrompt([]model.Message{
		msg(1, model.RoleUser, "分析启动变慢的原因"),
	})
	for _, must := range []string{"用户原始目标的准确表述", "已经排除的错误方向", "尚未完成的事项"} {
		if !strings.Contains(prompt, must) {
			t.Errorf("提示词应要求保留「%s」，实际提示词缺少该项", must)
		}
	}
	if !strings.Contains(prompt, "分析启动变慢的原因") {
		t.Error("提示词应包含原始对话内容")
	}
}

// TestSummaryPromptTruncatesToolOutput 工具输出在提示词里必须被截断。
//
// 否则「为了压缩而发的请求」本身就会被撑爆，形成反向浪费。
func TestSummaryPromptTruncatesToolOutput(t *testing.T) {
	huge := strings.Repeat("x", 10000)
	prompt := BuildSummaryPrompt([]model.Message{
		msg(1, model.RoleUser, "任务"),
		msg(2, model.RoleTool, huge),
	})
	if len(prompt) > 5000 {
		t.Errorf("提示词过长（%d 字符），工具输出未被有效截断", len(prompt))
	}
	if !utf8.ValidString(prompt) {
		t.Error("提示词必须是合法 UTF-8")
	}
}

// ---- History：摘要叠加 ----

// TestHistoryNoCompactionPassthrough 无压缩记录时原样返回。
func TestHistoryNoCompactionPassthrough(t *testing.T) {
	msgs := []model.Message{
		msg(1, model.RoleUser, "a"),
		msg(2, model.RoleAssistant, "b"),
	}
	out := History(msgs, nil)
	if len(out) != 2 {
		t.Fatalf("应返回 2 条，得到 %d", len(out))
	}
	if out[0].Content != "a" || out[1].Content != "b" {
		t.Error("内容应原样保留")
	}
}

// TestHistoryReplacesCompactedRange 已压缩区间被摘要替代。
func TestHistoryReplacesCompactedRange(t *testing.T) {
	msgs := []model.Message{
		msg(1, model.RoleUser, "旧任务"),
		msg(2, model.RoleAssistant, "旧回复"),
		msg(3, model.RoleUser, "新任务"),
	}
	comps := []model.Compaction{{
		StartSeq: 1, EndSeq: 2, SummaryMarkdown: "摘要内容", ReplacedMessages: 2,
	}}

	out := History(msgs, comps)

	// 期望：摘要 + 新任务 = 2 条
	if len(out) != 2 {
		t.Fatalf("应输出 2 条（摘要 + 新任务），得到 %d: %+v", len(out), out)
	}
	if !strings.Contains(out[0].Content, "摘要内容") {
		t.Errorf("首条应为摘要，得到 %q", out[0].Content)
	}
	if out[1].Content != "新任务" {
		t.Errorf("第二条应为未压缩消息，得到 %q", out[1].Content)
	}
	// 被压缩的原始内容不得再出现。
	for _, m := range out {
		if strings.Contains(m.Content, "旧回复") {
			t.Error("已压缩的消息不应出现在历史中")
		}
	}
}

// TestHistoryPreservesOrder 多条压缩记录的时间线顺序必须正确。
func TestHistoryPreservesOrder(t *testing.T) {
	msgs := []model.Message{
		msg(1, model.RoleUser, "r1"),
		msg(2, model.RoleAssistant, "a1"),
		msg(3, model.RoleUser, "r2"),
		msg(4, model.RoleAssistant, "a2"),
		msg(5, model.RoleUser, "r3"),
	}
	comps := []model.Compaction{
		{StartSeq: 1, EndSeq: 2, SummaryMarkdown: "S1", ReplacedMessages: 2},
		{StartSeq: 3, EndSeq: 4, SummaryMarkdown: "S2", ReplacedMessages: 2},
	}
	out := History(msgs, comps)

	if len(out) != 3 {
		t.Fatalf("应为 S1 + S2 + r3 = 3 条，得到 %d", len(out))
	}
	if !strings.Contains(out[0].Content, "S1") {
		t.Errorf("第 1 条应为 S1，得到 %q", out[0].Content)
	}
	if !strings.Contains(out[1].Content, "S2") {
		t.Errorf("第 2 条应为 S2，得到 %q", out[1].Content)
	}
	if out[2].Content != "r3" {
		t.Errorf("第 3 条应为 r3，得到 %q", out[2].Content)
	}
}

// TestSummaryMessageIsUserRole 摘要必须用 user 角色而非 system。
//
// 用 system 会让模型误以为这是新增的指令/约束，而不是"此前发生的事实"。
func TestSummaryMessageIsUserRole(t *testing.T) {
	m := summaryMessage(model.Compaction{StartSeq: 1, EndSeq: 5, SummaryMarkdown: "x"})
	if m.Role != llm.RoleUser {
		t.Errorf("摘要角色应为 user，得到 %s", m.Role)
	}
	if !strings.Contains(m.Content, "此前对话摘要") {
		t.Errorf("摘要应带明确标注，得到 %q", m.Content)
	}
}
