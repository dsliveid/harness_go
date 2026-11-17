package agent

import (
	"encoding/json"
	"testing"

	"harness-go/internal/llm"
	"harness-go/internal/model"
)

// ============ 工具调用声明与 tool 结果配对（防会话被永久污染） ============
//
// 背景（真实事故）：会话 15f526a5 的 assistant 消息 tool_calls_json 为 NULL，
// 而紧随的两条 tool 消息引用了从未被声明的 tool_call_id → 孤儿 tool 消息
// → 该会话后续每次请求都被上游 400（bad_request: 请求被上游拒绝），且永不恢复。
//
// 成因：llm.ToolCall.Arguments 是 json.RawMessage，非法 JSON 会让 json.Marshal
// 整段失败，旧代码用 `if err == nil` 把失败静默吞掉，声明随之丢失。
//
// 本文件锁定三条不变量：
//  1. 无论模型给出多畸形的参数，assistant 的 tool_calls 声明都必须落库（marshalToolCalls）；
//  2. 历史重放前必须清除孤儿 tool 消息（dropOrphanToolMessages）；
//  3. 合法历史必须原样保留（不能为了清理脏数据把正常对话也砍掉）。

// TestMarshalToolCallsNeverLosesDeclaration 畸形参数不得导致声明丢失。
func TestMarshalToolCallsNeverLosesDeclaration(t *testing.T) {
	cases := []struct {
		desc string
		args string
	}{
		{"正常对象", `{"path":"a.md"}`},
		{"空串（模型未给参数）", ``},
		{"仅左方括号（流式被截断）", `[`},
		{"截断的对象", `{"path":"a`},
		{"数组形态", `[{"path":"a"}]`},
	}
	for _, c := range cases {
		tcs := []llm.ToolCall{{ID: "call_1", Name: "read_file", Arguments: json.RawMessage(c.args)}}
		raw := marshalToolCalls(nil, RunRequest{SessionID: "s1", RunID: "r1"}, tcs)

		if len(raw) == 0 {
			t.Fatalf("%s: 声明不得为空（这正是孤儿 tool 消息的成因）", c.desc)
		}
		if !json.Valid(raw) {
			t.Fatalf("%s: 落库的 tool_calls 必须是合法 JSON，得到 %q", c.desc, string(raw))
		}
		var back []llm.ToolCall
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("%s: 反序列化失败: %v", c.desc, err)
		}
		if len(back) != 1 || back[0].ID != "call_1" || back[0].Name != "read_file" {
			t.Fatalf("%s: ID/Name 必须在归一化后保留，得到 %+v", c.desc, back)
		}
		// 参数即使被降级，也必须仍是合法 JSON（否则下一轮 marshal 又会炸）
		if !json.Valid(back[0].Arguments) {
			t.Fatalf("%s: 归一化后的 Arguments 必须合法，得到 %q", c.desc, string(back[0].Arguments))
		}
	}
}

// TestMarshalToolCallsPreservesValidArgs 合法参数必须原样保留，不得被改写。
func TestMarshalToolCallsPreservesValidArgs(t *testing.T) {
	orig := `{"path":"docs/a.md","max_lines":100}`
	tcs := []llm.ToolCall{{ID: "call_ok", Name: "read_file", Arguments: json.RawMessage(orig)}}
	raw := marshalToolCalls(nil, RunRequest{SessionID: "s1"}, tcs)

	var back []llm.ToolCall
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if string(back[0].Arguments) != orig {
		t.Fatalf("合法参数不应被改写，期望 %s，得到 %s", orig, string(back[0].Arguments))
	}
}

// TestDropOrphanToolMessagesRemovesUndeclared 无人声明的 tool 消息必须被丢弃。
//
// 这是本次事故的最小复现：assistant 是空消息（声明丢失），后面跟两条 tool 结果。
func TestDropOrphanToolMessagesRemovesUndeclared(t *testing.T) {
	in := []llm.Message{
		{Role: llm.RoleUser, Content: "执行测试"},
		{Role: llm.RoleAssistant, Content: ""}, // ← 声明丢失（tool_calls_json 为 NULL）
		{Role: llm.RoleTool, Content: "FAIL ...", ToolCallID: "call_e246224dd"},
		{Role: llm.RoleTool, Content: "参数不合法", ToolCallID: "call_96ead1384"},
	}
	out := dropOrphanToolMessages(in)

	if len(out) != 2 {
		t.Fatalf("孤儿 tool 消息应被丢弃，期望 2 条，得到 %d 条: %+v", len(out), out)
	}
	for _, m := range out {
		if m.Role == llm.RoleTool {
			t.Fatalf("输出中不应残留 tool 消息: %+v", m)
		}
	}
}

// TestDropOrphanToolMessagesKeepsValidPairs 合法的 assistant→tool 配对必须保留。
func TestDropOrphanToolMessagesKeepsValidPairs(t *testing.T) {
	tcs := []llm.ToolCall{
		{ID: "call_a", Name: "read_file", Arguments: json.RawMessage(`{"path":"a"}`)},
		{ID: "call_b", Name: "read_file", Arguments: json.RawMessage(`{"path":"b"}`)},
	}
	in := []llm.Message{
		{Role: llm.RoleUser, Content: "读两个文件"},
		{Role: llm.RoleAssistant, ToolCalls: tcs},
		{Role: llm.RoleTool, Content: "A 内容", ToolCallID: "call_a"},
		{Role: llm.RoleTool, Content: "B 内容", ToolCallID: "call_b"},
		{Role: llm.RoleAssistant, Content: "读完了"},
	}
	out := dropOrphanToolMessages(in)
	if len(out) != len(in) {
		t.Fatalf("合法历史不得被清理，期望 %d 条，得到 %d 条: %+v", len(in), len(out), out)
	}
}

// TestDropOrphanToolMessagesHandlesDuplicateAndMisordered 重复应答与顺序错乱也要清掉。
func TestDropOrphanToolMessagesHandlesDuplicateAndMisordered(t *testing.T) {
	tcs := []llm.ToolCall{{ID: "call_x", Name: "run_command", Arguments: json.RawMessage(`{}`)}}
	in := []llm.Message{
		{Role: llm.RoleTool, Content: "凭空出现的 tool（无前置 assistant）", ToolCallID: "call_x"},
		{Role: llm.RoleAssistant, ToolCalls: tcs},
		{Role: llm.RoleTool, Content: "首次应答", ToolCallID: "call_x"},
		{Role: llm.RoleTool, Content: "重复应答", ToolCallID: "call_x"},
	}
	out := dropOrphanToolMessages(in)

	var tools int
	for _, m := range out {
		if m.Role == llm.RoleTool {
			tools++
			if m.Content == "重复应答" || m.Content == "凭空出现的 tool（无前置 assistant）" {
				t.Fatalf("非法 tool 消息未被清除: %q", m.Content)
			}
		}
	}
	if tools != 1 {
		t.Fatalf("应只保留 1 条合法 tool 消息，得到 %d 条", tools)
	}
}

// TestDropOrphanToolMessagesStopsAtNewUserTurn 新 user 轮次之后的悬挂声明不再回溯配对。
func TestDropOrphanToolMessagesStopsAtNewUserTurn(t *testing.T) {
	tcs := []llm.ToolCall{{ID: "call_old", Name: "read_file", Arguments: json.RawMessage(`{}`)}}
	in := []llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: tcs}, // 声明后未被应答
		{Role: llm.RoleUser, Content: "算了，换个问题"},
		{Role: llm.RoleTool, Content: "迟到的结果", ToolCallID: "call_old"},
	}
	out := dropOrphanToolMessages(in)
	for _, m := range out {
		if m.Role == llm.RoleTool {
			t.Fatalf("跨轮次的 tool 消息不应被接受: %+v", m)
		}
	}
	if len(out) != 2 {
		t.Fatalf("user/assistant 应保留，期望 2 条，得到 %d 条", len(out))
	}
}

// TestLoadHistoryDropsOrphans 端到端：loadHistory 必须清掉库里的脏数据。
func TestLoadHistoryDropsOrphans(t *testing.T) {
	f := &fakeSessionDB{}
	// 事故现场复刻：空 assistant + 两条孤儿 tool
	f.messages = []*model.Message{
		{Seq: 1, Role: model.RoleUser, Content: "执行测试"},
		{Seq: 2, Role: model.RoleAssistant, Content: ""},
		{Seq: 3, Role: model.RoleTool, Content: "FAIL", ToolCallID: "call_e246224dd"},
		{Seq: 4, Role: model.RoleTool, Content: "参数不合法", ToolCallID: "call_96ead1384"},
	}
	e := &Engine{}
	got, err := e.loadHistory(t.Context(), f)
	if err != nil {
		t.Fatalf("loadHistory 失败: %v", err)
	}
	for _, m := range got {
		if m.Role == llm.RoleTool {
			t.Fatalf("loadHistory 必须过滤孤儿 tool 消息，仍残留: %+v", m)
		}
	}
}
