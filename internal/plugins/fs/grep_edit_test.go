package fs

import (
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
)

// TestGrepIncludeFiltersFiles include 必须真正起到文件名过滤作用。
//
// 改造前 grep 只有 pattern/path，模型想"只搜 .go 文件"只能先 glob 再逐个 read_file，
// 效率极低。include 补齐后，一次调用即可限定范围。
func TestGrepIncludeFiltersFiles(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("a.go", "func target() {}\n")
	_ = h.Seed("b.md", "target 出现在这里\n")
	_ = h.Seed("sub/c.go", "// target 在子目录\n")

	// 不限 include：三个文件都应命中
	res, _ := invokeGrep(t.Context(), h, call(t, `{"pattern":"target"}`))
	c := contentOf(t, res)
	for _, want := range []string{"a.go", "b.md"} {
		if !strings.Contains(c, want) {
			t.Fatalf("未过滤时应命中 %s: %q", want, c)
		}
	}

	// include=*.go：b.md 必须被排除，子目录 c.go 命中
	res, _ = invokeGrep(t.Context(), h, call(t, `{"pattern":"target","include":"*.go"}`))
	c = contentOf(t, res)
	if strings.Contains(c, "b.md") {
		t.Fatalf("include=*.go 不应命中 b.md: %q", c)
	}
	if !strings.Contains(c, "a.go") {
		t.Fatalf("include=*.go 应命中 a.go: %q", c)
	}
}

// TestGrepIncludeInvalidPattern include 语法错误必须显式报错（不能静默当没传）。
func TestGrepIncludeInvalidPattern(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("a.txt", "x\n")
	res, _ := invokeGrep(t.Context(), h, call(t, `{"pattern":"x","include":"[unclosed"}`))
	if !res.IsError || !strings.Contains(res.Content, "include 模式语法错误") {
		t.Fatalf("非法 include 必须报错: %q", res.Content)
	}
}

// TestGrepContextLines 上下文行必须包含前后文并标注命中行。
func TestGrepContextLines(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("ctx.txt", "l1\nl2\nHIT\nl4\nl5\n")

	res, _ := invokeGrep(t.Context(), h, call(t, `{"pattern":"HIT","context_lines":1}`))
	c := contentOf(t, res)
	if !strings.Contains(c, "l2") || !strings.Contains(c, "l4") {
		t.Fatalf("context_lines=1 应含前后各一行: %q", c)
	}
	if strings.Contains(c, "l1") || strings.Contains(c, "l5") {
		t.Fatalf("context_lines=1 不应含更远的行: %q", c)
	}
	if !strings.Contains(c, "> ") {
		t.Fatalf("命中行应以 > 标注: %q", c)
	}
}

// TestGrepContextLinesClamped context_lines 必须硬性夹在 0~5。
func TestGrepContextLinesClamped(t *testing.T) {
	h := plugintest.New(t)
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		b.WriteString("line\n")
	}
	b.WriteString("TARGET\n")
	_ = h.Seed("big.txt", b.String())

	res, _ := invokeGrep(t.Context(), h, call(t, `{"pattern":"TARGET","context_lines":50}`))
	c := contentOf(t, res)
	// 夹到 5：最多出现 11 行（5 前 + 命中 + 5 后），不应把 20 行全带上
	if strings.Count(c, "line") != 5 {
		t.Fatalf("context_lines 应夹到 5（前 5 行）: %q", c)
	}
}

// TestEditFileReplaceAll replace_all=true 时不再要求唯一命中。
func TestEditFileReplaceAll(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("r.txt", "foo foo foo\n")

	// 默认：多次命中必须报错并提示 replace_all
	res, _ := invokeEditFile(t.Context(), h, call(t,
		`{"path":"r.txt","old_string":"foo","new_string":"bar"}`))
	if !res.IsError || !strings.Contains(res.Content, "replace_all=true") {
		t.Fatalf("多次命中应报错并提示 replace_all: %q", res.Content)
	}

	// replace_all=true：全部替换
	res, _ = invokeEditFile(t.Context(), h, call(t,
		`{"path":"r.txt","old_string":"foo","new_string":"bar","replace_all":true}`))
	if res.IsError {
		t.Fatalf("replace_all 不应失败: %q", res.Content)
	}
	data, err := h.ReadFile(t.Context(), "r.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "bar bar bar\n" {
		t.Fatalf("全量替换结果错误: %q", string(data))
	}
}

// TestReadFileSchemaMatchesImpl read_file 的 schema 参数名必须与实现读取的字段一致。
//
// 这是对"文本/schema 声明了某参数，实现却读另一个名字"这类静默失效的直接防护：
// 宽松 json.Unmarshal 会丢弃未知字段，导致参数"看起来传了、实际没生效"。
func TestReadFileSchemaMatchesImpl(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("s.txt", "1\n2\n3\n4\n5\n")

	tools := Manifest().Tools
	var schemaJSON string
	for _, tl := range tools {
		if tl.Name == "read_file" {
			schemaJSON = string(tl.SchemaJSON)
		}
	}
	if !strings.Contains(schemaJSON, "offset_line") || !strings.Contains(schemaJSON, "max_lines") {
		t.Fatalf("manifest 应声明 offset_line/max_lines: %s", schemaJSON)
	}
	res, _ := invokeReadFile(t.Context(), h, call(t, `{"path":"s.txt","offset_line":2,"max_lines":2}`))
	c := contentOf(t, res)
	if !strings.Contains(c, "     2\t2") || !strings.Contains(c, "     3\t3") {
		t.Fatalf("manifest 声明的参数必须真正生效: %q", c)
	}
	if strings.Contains(c, "     4\t4") {
		t.Fatalf("max_lines=2 不应读到第 4 行: %q", c)
	}
}
