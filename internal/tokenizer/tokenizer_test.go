package tokenizer

import "testing"

func TestHeuristicCountText(t *testing.T) {
	h := NewHeuristic()
	if got := h.CountText(""); got != 0 {
		t.Fatalf("空串应为 0，得到 %d", got)
	}
	// 4 个 ASCII 字符 ≈ 1 token
	if got := h.CountText("abcd"); got != 1 {
		t.Fatalf("abcd 应约 1 token，得到 %d", got)
	}
	// 中文 1 字 ≈ 1 token
	if got := h.CountText("你好世界"); got != 4 {
		t.Fatalf("4 个汉字应约 4 token，得到 %d", got)
	}
	// 中英混合
	if got := h.CountText("你好abcd"); got != 3 { // 2 + 1
		t.Fatalf("混合应约 3 token，得到 %d", got)
	}
	if !h.Estimated() {
		t.Fatal("heuristic 必须标记为估算")
	}
	if h.Name() != "heuristic" {
		t.Fatalf("Name 错误: %s", h.Name())
	}
}

// 结构开销：每条消息 +3，末尾引导 +3。
func TestHeuristicCountMessagesIncludesOverhead(t *testing.T) {
	h := NewHeuristic()
	msgs := []Message{
		{Role: "user", Content: "abcd"},      // 3 + 1("user") + 1("abcd")
		{Role: "assistant", Content: "abcd"}, // 3 + 3("assistant"=9字符/4) + 1("abcd")
	}
	got := h.CountMessages(msgs)
	// 末尾引导 3 + (3+1+1) + (3+3+1) = 3 + 5 + 7 = 15
	if got != 15 {
		t.Fatalf("消息段计数应含结构开销，期望 15，得到 %d", got)
	}
	if h.CountMessages(nil) != replyPriming {
		t.Fatalf("空消息也应含末尾引导开销: %d", h.CountMessages(nil))
	}
}

// tools 的 schema 必须全额计入（最容易被漏算的部分）。
func TestHeuristicCountToolsIncludesSchema(t *testing.T) {
	h := NewHeuristic()
	small := []ToolDef{{Name: "echo", Description: "回显", SchemaJSON: "{}"}}
	big := []ToolDef{{Name: "echo", Description: "回显", SchemaJSON: `{"type":"object","properties":{"a":{"type":"string","description":"一个比较长的参数说明"}}}`}}
	if h.CountTools(big) <= h.CountTools(small) {
		t.Fatal("schema 变大后计数必须增加（不能漏算 tools schema）")
	}
}

func TestRegistryMatching(t *testing.T) {
	r := NewRegistry()

	// 未注册 → 回退 heuristic
	if got := r.For("unknown-model").Name(); got != "heuristic" {
		t.Fatalf("未注册模型应回退 heuristic，得到 %s", got)
	}

	custom := &fakeCounter{name: "tiktoken:o200k_base"}
	r.Register("gpt-4o", custom)
	if got := r.For("gpt-4o"); got.Name() != "tiktoken:o200k_base" {
		t.Fatalf("精确匹配失败: %s", got.Name())
	}
	// 大小写不敏感
	if got := r.For("GPT-4O"); got.Name() != "tiktoken:o200k_base" {
		t.Fatalf("精确匹配应大小写不敏感: %s", got.Name())
	}

	// 前缀匹配
	prefixCounter := &fakeCounter{name: "hf:qwen"}
	r.RegisterPrefix("qwen", prefixCounter)
	if got := r.For("qwen2.5-72b-instruct"); got.Name() != "hf:qwen" {
		t.Fatalf("前缀匹配失败: %s", got.Name())
	}

	// 最长前缀优先
	longest := &fakeCounter{name: "hf:qwen-long"}
	r.RegisterPrefix("qwen2.5", longest)
	if got := r.For("qwen2.5-72b"); got.Name() != "hf:qwen-long" {
		t.Fatalf("最长前缀应优先: %s", got.Name())
	}
	// 不匹配的仍走短前缀
	if got := r.For("qwen-turbo"); got.Name() != "hf:qwen" {
		t.Fatalf("短前缀应仍生效: %s", got.Name())
	}

	// 自定义回退
	fb := &fakeCounter{name: "custom-fallback"}
	r.SetFallback(fb)
	if got := r.For("nope"); got.Name() != "custom-fallback" {
		t.Fatalf("回退覆盖失败: %s", got.Name())
	}
}

func TestRegistryNeverReturnsNil(t *testing.T) {
	r := NewRegistry()
	if r.For("") == nil {
		t.Fatal("空模型名也必须返回非 nil 计数器")
	}
	r.Register("", &fakeCounter{name: "x"}) // 应被忽略
	if r.For("x") == nil {
		t.Fatal("应返回回退计数器")
	}
}

type fakeCounter struct{ name string }

func (f *fakeCounter) Name() string                { return f.name }
func (f *fakeCounter) Estimated() bool             { return false }
func (f *fakeCounter) CountText(string) int        { return 0 }
func (f *fakeCounter) CountMessages([]Message) int { return 0 }
func (f *fakeCounter) CountTools([]ToolDef) int    { return 0 }
