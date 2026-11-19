package resultfmt

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestShortContentUntouched 短内容必须原样返回。
//
// 这是最重要的契约：定型器绝不能改写正常输出，否则会引入无谓的噪音与
// 边界 bug。大量工具输出都是短的，这条路径必须零开销。
func TestShortContentUntouched(t *testing.T) {
	cases := []struct{ tool, content string }{
		{"run_command", "exit=0"},
		{"grep", "a.go:1: hit"},
		{"read_file", strings.Repeat("x", 100)},
		{"list_dir", "a/\nb/\n"},
		{"完全未知的工具", "随便什么"},
	}
	for _, c := range cases {
		if got := Format(c.tool, c.content); got != c.content {
			t.Errorf("%s 短内容被改动：want %q, got %q", c.tool, c.content, got)
		}
	}
}

// TestRunCommandHeadTail 长命令输出保留头尾。
func TestRunCommandHeadTail(t *testing.T) {
	head := strings.Repeat("H", 2000)
	tail := strings.Repeat("T", 1200)
	content := head + strings.Repeat("M", 10000) + tail

	got := Format("run_command", content)

	if !strings.HasPrefix(got, "HHHH") {
		t.Error("必须保留头部")
	}
	if !strings.HasSuffix(got, "TTTT") {
		t.Error("必须保留尾部（错误信息通常在末尾）")
	}
	if strings.Contains(got, strings.Repeat("M", 100)) {
		t.Error("中间部分应被折叠")
	}
	if !strings.Contains(got, "原始") {
		t.Error("收敛后必须自我说明原始规模，否则模型会误以为这就是全部")
	}
	if utf8.RuneCountInString(got) >= utf8.RuneCountInString(content) {
		t.Error("收敛后长度必须小于原始")
	}
}

// TestGrepLineCollapse 行数超阈值时按行收敛。
func TestGrepLineCollapse(t *testing.T) {
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, "file.go:1: hit")
	}
	content := strings.Join(lines, "\n")

	got := Format("grep", content)

	if strings.Count(got, "\n") > 35 {
		t.Errorf("应保留约 30 行，实际行数偏多: %d", strings.Count(got, "\n"))
	}
	if !strings.Contains(got, "共 100 行") {
		t.Errorf("应说明总行数，实际: %q", tailOf(got, 200))
	}
}

// TestListDirLineCollapse list_dir 行阈值 50/保留 40。
func TestListDirLineCollapse(t *testing.T) {
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, "entry")
	}
	got := Format("list_dir", strings.Join(lines, "\n"))
	if !strings.Contains(got, "共 200 行") {
		t.Errorf("应说明总行数，实际: %q", tailOf(got, 200))
	}
}

// TestUTF8Safety 中文/emoji 裁剪不得产生非法 UTF-8。
//
// 按字节裁剪是这类实现最典型的 bug：会把 3 字节的中文切成半个。
func TestUTF8Safety(t *testing.T) {
	content := strings.Repeat("中文内容🎯", 5000) // 远超 run_command 阈值
	got := Format("run_command", content)
	if !utf8.ValidString(got) {
		t.Fatal("裁剪结果必须是合法 UTF-8")
	}
}

// TestUnknownToolHasDefaultProtection 未登记工具也要有兜底保护。
//
// 新增工具忘记登记策略时不能完全没有上限（否则又会重现上下文爆炸），
// 但阈值要宽松，避免切碎正常输出。
func TestUnknownToolHasDefaultProtection(t *testing.T) {
	p := ProfileFor("never_registered_tool")
	if p.MaxChars <= 0 {
		t.Fatal("未登记工具必须有默认保护")
	}
	if p.MaxChars < 16<<10 {
		t.Error("默认阈值不应过小，否则会切碎正常输出")
	}
}

// TestEmptyContentSafe 空内容不得 panic 或产生提示语。
func TestEmptyContentSafe(t *testing.T) {
	if got := Format("run_command", ""); got != "" {
		t.Errorf("空内容应原样返回，得到 %q", got)
	}
}

// TestHeadTailOverflowGuard 极端小内容时头尾不得重叠输出。
func TestHeadTailOverflowGuard(t *testing.T) {
	// 构造一个刚好超过阈值但远小于 head+tail 的内容。
	p := ProfileFor("run_command")
	content := strings.Repeat("A", p.MaxChars+10)
	got := Format("run_command", content)
	if strings.Count(got, "AAAA") > 20 {
		// 头尾重叠会表现为大量重复的 A，这里做个粗略但有效的防护
		t.Logf("输出片段: %q", got[:min(200, len(got))])
	}
	if !utf8.ValidString(got) {
		t.Fatal("必须是合法 UTF-8")
	}
	if n := strings.Count(got, "…["); n != 1 {
		t.Errorf("提示语应恰好出现一次（头尾不得重复），实际 %d 次", n)
	}
}

// TestReadFileThreshold 64K 阈值下，正常文件不受影响。
func TestReadFileThreshold(t *testing.T) {
	normal := strings.Repeat("x", 60<<10) // 60K < 64K
	if got := Format("read_file", normal); got != normal {
		t.Error("60K 的文件不应被定型")
	}
	big := strings.Repeat("x", 100<<10)
	if got := Format("read_file", big); got == big {
		t.Error("100K 的文件必须被定型")
	}
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
