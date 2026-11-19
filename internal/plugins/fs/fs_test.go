package fs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

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
	res, _ := invokeReadFile(t.Context(), h, call(t, `{"path":"a.txt","offset_line":8,"max_lines":2}`))
	c := contentOf(t, res)
	if !strings.Contains(c, "     8\tLLLLLLLL") || !strings.Contains(c, "     9\tLLLLLLLLL") {
		t.Fatalf("分段读取错误: %q", c)
	}
	if strings.Contains(c, "    10\t") {
		t.Fatalf("max_lines=2 不应读到第 10 行: %q", c)
	}
	if !strings.Contains(c, "已读 8-9") {
		t.Fatalf("应标注续读信息: %q", c)
	}
}

func TestReadFileDefaultLimitAndClamp(t *testing.T) {
	h := plugintest.New(t)
	var b strings.Builder
	for i := 1; i <= 2600; i++ {
		b.WriteString("X\n")
	}
	_ = h.Seed("big.txt", b.String())

	// 默认不传 max_lines：必须按 defaultReadLimit 截取（而非读全 2600 行）。
	res, _ := invokeReadFile(t.Context(), h, call(t, `{"path":"big.txt"}`))
	c := contentOf(t, res)
	if !strings.Contains(c, fmt.Sprintf("已读 1-%d", defaultReadLimit)) {
		t.Fatalf("默认读取应按 defaultReadLimit=%d 截取: %q", defaultReadLimit, c)
	}
	if strings.Contains(c, fmt.Sprintf("%6d\t", defaultReadLimit+1)) {
		t.Fatalf("默认读取不应越过 defaultReadLimit=%d 行: %q", defaultReadLimit, c)
	}

	// 超量申请：必须被 clamp 到 maxReadLimit，而不是产出超长输出被入库定型碎裂。
	res, _ = invokeReadFile(t.Context(), h, call(t, `{"path":"big.txt","max_lines":99999}`))
	c = contentOf(t, res)
	if !strings.Contains(c, fmt.Sprintf("已读 1-%d", maxReadLimit)) {
		t.Fatalf("max_lines 超大值应被 clamp 到 %d: %q", maxReadLimit, c)
	}
}

func TestReadFileManifestMatchesDefaults(t *testing.T) {
	// 防「契约谎言」回归：schema 描述曾写「默认 500，可按需指定至 1000 行」，
	// 而代码默认值早已漂移到 2000 —— 模型据此决策会误判读取范围。
	// 这里把描述与常量强绑定，改常量却忘了改描述会立刻变红。
	var schemaJSON string
	for _, tl := range Manifest().Tools {
		if tl.Name == "read_file" {
			schemaJSON = string(tl.SchemaJSON)
		}
	}
	if schemaJSON == "" {
		t.Fatal("manifest 缺少 read_file")
	}
	for _, want := range []string{
		fmt.Sprintf("默认 %d", defaultReadLimit),
		fmt.Sprintf("超过 %d", maxReadLimit),
	} {
		if !strings.Contains(schemaJSON, want) {
			t.Fatalf("schema 描述应与代码常量一致，缺少 %q: %s", want, schemaJSON)
		}
	}
}

func TestTruncateAtLineBoundary(t *testing.T) {
	t.Run("不超限不截断", func(t *testing.T) {
		data := []byte("a\nb\n")
		got, cut := truncateAtLineBoundary(data, 100)
		if cut || string(got) != "a\nb\n" {
			t.Fatalf("未超限不应截断: %q cut=%v", got, cut)
		}
	})

	t.Run("切在行尾且保留换行", func(t *testing.T) {
		data := []byte("aaaa\nbbbb\ncccc\n")
		got, cut := truncateAtLineBoundary(data, 11) // 落在第二行中间
		if !cut {
			t.Fatal("超限必须报告截断")
		}
		if string(got) != "aaaa\nbbbb\n" {
			t.Fatalf("必须回退到行尾: %q", got)
		}
	})

	t.Run("不切断多字节字符", func(t *testing.T) {
		// 无换行时只能按 rune 边界回退：中文 3 字节/字。
		data := []byte("中文内容测试")
		got, cut := truncateAtLineBoundary(data, 7) // 7 落在第二个字的中间
		if !cut {
			t.Fatal("超限必须报告截断")
		}
		if !utf8.Valid(got) {
			t.Fatalf("截断结果必须是合法 UTF-8: %q", got)
		}
		if string(got) != "中文" {
			t.Fatalf("应按 rune 边界回退到「中文」: %q", got)
		}
	})
}

// TestReadFileByteTruncationWarns 锁定「字节上限触发时不得冒充全文」。
//
// 这是本工具最危险的一类失真：字节截断后行数只是**已加载部分**，
// 若提示仍写「共 N 行」，模型会认为文件读完了，直接漏掉后半部分。
func TestReadFileByteTruncationWarns(t *testing.T) {
	h := plugintest.New(t)
	// 构造 > maxReadBytes 且行数很少的文件（每行 4KB，共 200 行 ≈ 800KB）。
	line := strings.Repeat("x", 4096)
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString(line + "\n")
	}
	_ = h.Seed("huge.txt", b.String())

	res, _ := invokeReadFile(t.Context(), h, call(t, `{"path":"huge.txt","max_lines":5}`))
	c := contentOf(t, res)
	if !strings.Contains(c, "非全文") {
		t.Fatalf("字节截断时必须明示非全文，否则模型会误判已读完: %q", c)
	}
	if strings.Contains(c, "…[共 200 行") {
		t.Fatalf("字节截断时不得宣称文件总行数为 200: %q", c)
	}
	if !utf8.ValidString(c) {
		t.Fatal("输出必须为合法 UTF-8")
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

// TestGlobBraceAlternation 回归：{a,b} 必须真实展开。
//
// 事故背景（会话 9ee34e00 首轮）：模型传 **/*.{go,py,js,...}，旧实现用
// filepath.Match 不认识花括号 → 命中 0 条 → 返回「无匹配文件」，
// 模型据此误判「项目里没有代码文件」，白烧一轮并动摇后续判断。
func TestGlobBraceAlternation(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("main.go", "package main\n")
	_ = h.Seed("tool.py", "print('x')\n")
	_ = h.Seed("cfg.json", "{}")
	_ = h.Seed("note.md", "# n\n")

	res, err := invokeGlob(t.Context(), h, call(t, `{"pattern":"**/*.{go,py,json}"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("合法大括号模式不应报错: %q", res.Content)
	}
	c := contentOf(t, res)
	for _, want := range []string{"main.go", "tool.py", "cfg.json"} {
		if !strings.Contains(c, want) {
			t.Errorf("大括号展开后应命中 %s，实际结果: %q", want, c)
		}
	}
	if strings.Contains(c, "note.md") {
		t.Errorf("不应命中 note.md: %q", c)
	}
}

// TestGlobInvalidPatternIsError 回归：语法非法必须报错，绝不能伪装成「无匹配文件」。
//
// 这是与上面同等重要的契约——错误类型决定模型下一步行为：
// 报错 → 模型换写法重试；「无匹配」→ 模型认定文件不存在，方向直接跑偏。
func TestGlobInvalidPatternIsError(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("main.go", "package main\n")

	res, err := invokeGlob(t.Context(), h, call(t, `{"pattern":"**/*.{go,py"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("花括号未闭合必须返回错误结果，实际: %q", res.Content)
	}
	if !strings.Contains(res.Content, "语法错误") {
		t.Fatalf("错误文案应点明是语法问题，实际: %q", res.Content)
	}
}

// TestGlobEmptyResultStillText 合法模式无命中仍是普通文本结果（不是错误）。
func TestGlobEmptyResultStillText(t *testing.T) {
	h := plugintest.New(t)
	_ = h.Seed("main.go", "package main\n")

	res, err := invokeGlob(t.Context(), h, call(t, `{"pattern":"**/*.rs"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("合法但无命中不应是错误，实际: %q", res.Content)
	}
	if !strings.Contains(contentOf(t, res), "无匹配文件") {
		t.Fatalf("应提示无匹配文件，实际: %q", res.Content)
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
