package fs

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

func call(t *testing.T, raw string) *toolapi.Call {
	t.Helper()
	return &toolapi.Call{Args: []byte(raw)}
}

func contentOf(t *testing.T, res *toolapi.Result) string {
	t.Helper()
	if res == nil {
		t.Fatal("结果为 nil")
	}
	return res.Content
}

func TestReadFileLineNumbers(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("a.txt", "第一行\n第二行\n第三行\n")
	res, err := invokeReadFile(t.Context(), h, call(t, `{"path":"a.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	c := contentOf(t, res)
	if !strings.Contains(c, "     1\t第一行") || !strings.Contains(c, "     3\t第三行") {
		t.Fatalf("行号标注错误: %q", c)
	}
}

func TestReadFileOffsetLimit(t *testing.T) {
	h := plugintest.New(t)
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		b.WriteString(strings.Repeat("L", i) + "\n")
	}
	_ = h.Seed("a.txt", b.String())
	res, _ := invokeReadFile(t.Context(), h, call(t, `{"path":"a.txt","offset":8,"limit":2}`))
	c := contentOf(t, res)
	if !strings.Contains(c, "     8\tLLLLLLLL") || !strings.Contains(c, "     9\tLLLLLLLLL") {
		t.Fatalf("分段读取错误: %q", c)
	}
	if strings.Contains(c, "    10\t") {
		t.Fatalf("limit=2 不应读到第 10 行: %q", c)
	}
	if !strings.Contains(c, "已读 8-9") {
		t.Fatalf("应标注续读信息: %q", c)
	}
}

func TestWriteFileProducesDiff(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("a.txt", "old1\nold2\n")
	res, err := invokeWriteFile(t.Context(), h, call(t, `{"path":"a.txt","content":"old1\nnew2\nnew3\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "-old2") || !strings.Contains(res.Content, "+new2") {
		t.Fatalf("diff 内容错误: %q", res.Content)
	}
	// 结构化 diff 进 Meta.data（前端 FileDiff 契约；动态类型是 json.RawMessage）
	rawData, ok := res.Meta["data"]
	if !ok || !strings.Contains(fmt.Sprintf("%s", rawData), "\"diff\"") {
		t.Fatalf("缺少结构化 diff（Meta.data）: %v", res.Meta)
	}
	if !strings.Contains(fmt.Sprintf("%s", rawData), "\"path\":\"a.txt\"") {
		t.Fatalf("结构化 diff 内容错误: %s", rawData)
	}
	if !strings.Contains(res.Content, "+2/-1 行") {
		t.Fatalf("增删统计错误: %q", res.Content)
	}
	// 文件确实写入
	got, _ := h.ReadFile(t.Context(), "a.txt")
	if string(got) != "old1\nnew2\nnew3\n" {
		t.Fatalf("写入内容错误: %q", got)
	}
}

func TestEditFileUniqueMatch(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("a.txt", "func main() {\n\tfmt.Println(\"hello\")\n}\n")
	res, err := invokeEditFile(t.Context(), h, call(t,
		`{"path":"a.txt","old_string":"Println(\"hello\")","new_string":"Println(\"world\")"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Content, "+	fmt.Println(\"world\")") {
		t.Fatalf("编辑错误: %q", res.Content)
	}
	got, _ := h.ReadFile(t.Context(), "a.txt")
	if !strings.Contains(string(got), "world") {
		t.Fatal("编辑未生效")
	}
}

func TestEditFileNonUniqueRejected(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("a.txt", "x\nx\nx\n")
	res, _ := invokeEditFile(t.Context(), h, call(t, `{"path":"a.txt","old_string":"x","new_string":"y"}`))
	if !res.IsError || !strings.Contains(res.Content, "命中 3 次") {
		t.Fatalf("非唯一命中必须拒绝: %q", res.Content)
	}
}

func TestEditFileNoMatchRejected(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("a.txt", "abc\n")
	res, _ := invokeEditFile(t.Context(), h, call(t, `{"path":"a.txt","old_string":"zzz","new_string":"y"}`))
	if !res.IsError || !strings.Contains(res.Content, "未命中") {
		t.Fatalf("未命中必须拒绝: %q", res.Content)
	}
}

func TestGlobCrossDirectory(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("a/x.go", "package x\n")
	_ = h.Seed("a/b/y.go", "package y\n")
	_ = h.Seed("a/b/c.md", "# doc\n")
	res, err := invokeGlob(t.Context(), h, call(t, `{"pattern":"**/*.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	c := contentOf(t, res)
	if !strings.Contains(c, "a/x.go") || !strings.Contains(c, "a/b/y.go") || strings.Contains(c, "c.md") {
		t.Fatalf("glob 结果错误: %q", c)
	}
}

func TestGrepLiteralAndRegex(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("code.go", "func main() {}\n// TODO: fix\n")
	res, err := invokeGrep(t.Context(), h, call(t, `{"pattern":"TODO","literal":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(contentOf(t, res), "code.go:2:") {
		t.Fatalf("字面量检索错误: %q", res.Content)
	}
	// 正则
	res, _ = invokeGrep(t.Context(), h, call(t, `{"pattern":"func \\w+"}`))
	if !strings.Contains(contentOf(t, res), "code.go:1:") {
		t.Fatalf("正则检索错误: %q", res.Content)
	}
}

func TestGrepSkipsVendoredDirs(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("node_modules/pkg/x.js", "TODO inside\n")
	_ = h.Seed("src/a.js", "// TODO real\n")
	res, _ := invokeGrep(t.Context(), h, call(t, `{"pattern":"TODO"}`))
	c := contentOf(t, res)
	if strings.Contains(c, "node_modules") {
		t.Fatalf("node_modules 必须跳过: %q", c)
	}
	if !strings.Contains(c, "src/a.js") {
		t.Fatalf("正常文件必须命中: %q", c)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	h := plugintest.New(t)
	for _, tc := range []struct {
		tool string
		raw  string
	}{
		{"read_file", `{"path":"../../etc/passwd"}`},
		{"write_file", `{"path":"../evil.txt","content":"x"}`},
		{"read_file", `{"path":"C:\\Windows\\system32\\config"}`},
	} {
		var fn func(context.Context, toolapi.Host, *toolapi.Call) (*toolapi.Result, error)
		switch tc.tool {
		case "read_file":
			fn = invokeReadFile
		case "write_file":
			fn = invokeWriteFile
		}
		res, _ := fn(t.Context(), h, call(t, tc.raw))
		if !res.IsError {
			t.Fatalf("%s 越界路径必须拒绝: %s", tc.tool, tc.raw)
		}
	}
}

func TestFileOutlineMarkdownAndGo(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("doc.md", "# 标题\n正文\n## 小节\n内容\n")
	res, err := invokeFileOutline(t.Context(), h, call(t, `{"path":"doc.md"}`))
	if err != nil {
		t.Fatal(err)
	}
	c := contentOf(t, res)
	if !strings.Contains(c, "# 标题") || !strings.Contains(c, "## 小节") {
		t.Fatalf("Markdown 大纲错误: %q", c)
	}
	_ = h.Seed("main.go", "package main\n\nfunc main() {\n}\n\ntype T struct {\n}\n")
	res, _ = invokeFileOutline(t.Context(), h, call(t, `{"path":"main.go"}`))
	c = contentOf(t, res)
	if !strings.Contains(c, "func main()") || !strings.Contains(c, "type T struct") {
		t.Fatalf("Go 大纲错误: %q", c)
	}
}
