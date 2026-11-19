package prompt

// 缓存命中红线的回归测试。
//
// # 为什么这组断言必须存在（2026-11 事故取证）
//
// 引擎每步入参 ActiveTools（动态检索命中的 deferred 工具）都不相同。
// 旧实现把命中项从 deferred 里抠掉再渲染 system 的「## 可按需调用的工具」索引段，
// 于是**同一次 Run 内相邻两步的 system 字节不同**。system 是整段上下文的前缀，
// 它一变，后面所有 messages 的 KV 缓存全部击穿 —— 而 system + history 恰恰最长。
//
// 老版本 harness_mini 的 messages[0] 是纯静态 System Prompt
// （src-tauri/src/context/message_stream.rs:28 不可变前缀五准则之一）。
// 本项目的对齐方式：promote 只影响 Request.Tools（真正下发 schema 的部分），
// 索引段永远渲染全量 deferred。

import (
	"strings"
	"testing"

	"harness-go/internal/llm"
	"harness-go/internal/tokenizer"
)

// TestSystemPrefixStableAcrossActiveTools system 必须与 ActiveTools 无关。
//
// 这是「前缀缓存命中」的核心不变量：同一 Run 的每一步都必须拿到字节相同的 system。
func TestSystemPrefixStableAcrossActiveTools(t *testing.T) {
	b := NewBuilder(tokenizer.NewHeuristic(), DefaultLimits())

	// 三步分别命中不同的 deferred 工具（真实场景：模型每步检索到的关键词都不同）。
	steps := [][]string{
		nil,
		{"web_fetch"},
		{"plan"},
		{"plan", "web_fetch"},
	}

	var first string
	for i, active := range steps {
		in := baseInput()
		in.ActiveTools = active
		got := b.Build(in).Request.System
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("第 %d 步 system 与首步不一致（击穿前缀缓存）\n--- 首步 ---\n%s\n--- 本步 ---\n%s",
				i, first, got)
		}
	}
}

// TestPromoteStillAffectsToolSpecs promote 必须仍然生效（只作用于全量定义轨）。
//
// 缓存红线不能把 D-2 的 Token 收益改没了：命中项必须出现在 Request.Tools 里，
// 否则模型看不到它的 schema，promote 就成了空转。
func TestPromoteStillAffectsToolSpecs(t *testing.T) {
	b := NewBuilder(tokenizer.NewHeuristic(), DefaultLimits())
	base := len(b.Build(baseInput()).Request.Tools)

	in := baseInput()
	in.ActiveTools = []string{"web_fetch"}
	res := b.Build(in)
	if len(res.Request.Tools) != base+1 {
		t.Fatalf("命中项应追加进全量定义轨：期望 %d，得到 %d", base+1, len(res.Request.Tools))
	}
	var found bool
	for _, tl := range res.Request.Tools {
		if tl.Name == "web_fetch" {
			found = true
			if strings.TrimSpace(tl.Description) == "" {
				t.Fatal("提升后必须携带完整描述（否则模型无法判断该工具用途）")
			}
		}
	}
	if !found {
		t.Fatal("web_fetch 未进入全量定义轨，promote 空转")
	}
	// 索引报告仍应排除已提升项（它表达的是「还需 lookup_tool 才能看 schema 的那批」）。
	for _, n := range res.IndexedDeferred {
		if n == "web_fetch" {
			t.Fatal("已提升项不应出现在 IndexedDeferred（该字段表达「未注入 schema」的集合）")
		}
	}
}

// TestTrimHistoryIsTurnAtomic 历史裁剪必须是**轮次级**原子裁切。
//
// # 旧实现的真实缺陷
//
// 从最旧开始「每轮丢 1/4 条」，切点落在任意位置 —— 可能把 assistant(tool_calls)
// 与配对 tool 结果切开：孤儿 tool 消息让上游 400，且切点随预算波动 → 前缀缓存全废。
//
// 断言两条：保留段必须以 user 开头；不得出现「有 tool_calls 但无配对 tool」的尾部。
func TestTrimHistoryIsTurnAtomic(t *testing.T) {
	b := NewBuilder(tokenizer.NewHeuristic(), Limits{MaxHistory: 40})
	// 构造 4 轮：每轮 user + assistant(tool_calls) + tool
	var hist []llm.Message
	for i := 0; i < 4; i++ {
		hist = append(hist,
			llm.Message{Role: llm.RoleUser, Content: strings.Repeat("问", 20)},
			llm.Message{Role: llm.RoleAssistant, Content: "", ToolCalls: []llm.ToolCall{
				{ID: "c" + string(rune('0'+i)), Name: "read_file", Arguments: []byte("{}")},
			}},
			llm.Message{Role: llm.RoleTool, Content: strings.Repeat("答", 20), ToolCallID: "c" + string(rune('0'+i))},
		)
	}
	out, dropped := b.trimHistory(hist, 60)
	if dropped == 0 {
		t.Fatal("超预算时必须真的丢弃（否则本条测试没有验证力）")
	}
	if len(out) == 0 {
		t.Fatal("保留段不应为空（最后一轮必须留下）")
	}
	if out[0].Role != llm.RoleUser {
		t.Fatalf("保留段必须以 user 开头（轮次级裁切），实际首条 role=%s", out[0].Role)
	}
	// 丢掉的消息数必须是**整轮**（每轮 3 条）。
	if dropped%3 != 0 {
		t.Fatalf("丢弃数应为整轮倍数（每轮 3 条），实际 %d", dropped)
	}
	// 尾部不得留下悬空 tool_calls。
	last := out[len(out)-1]
	if last.Role == llm.RoleAssistant && len(last.ToolCalls) > 0 {
		t.Fatal("保留段末条不得是「有 tool_calls 但无配对 tool」的 assistant")
	}
}
