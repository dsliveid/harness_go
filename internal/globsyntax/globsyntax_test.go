package globsyntax

import (
	"errors"
	"strings"
	"testing"
)

// TestAlternation 锁定本次重构的核心动机：{a,b} 必须真实展开。
//
// 回归背景：会话 9ee34e00 首轮传 **/*.{go,py,js,ts,json,yaml,yml,toml} 得到
// 「无匹配文件」，模型据此误判项目不含代码。此用例若失败，说明又退回了
// filepath.Match 语义。
func TestAlternation(t *testing.T) {
	p := MustCompile("**/*.{go,py,json}")

	for _, want := range []string{"main.go", "a/b/c.go", "x.py", "deep/nest/dir/x.json"} {
		if !p.Match(want) {
			t.Errorf("应匹配 %q，实际未匹配（模式 %s，正则 %s）", want, p.Raw, p.Regexp())
		}
	}
	for _, un := range []string{"main.rs", "notes.txt", "a/b.go.txt"} {
		if p.Match(un) {
			t.Errorf("不应匹配 %q", un)
		}
	}
}

// TestAlternationWithWildcard 备选项自身可含 * 与 ?。
func TestAlternationWithWildcard(t *testing.T) {
	p := MustCompile("src/{*.go,t?st.go}")
	for _, want := range []string{"src/a.go", "src/test.go"} {
		if !p.Match(want) {
			t.Errorf("应匹配 %q", want)
		}
	}
	if p.Match("src/a.py") {
		t.Error("不应匹配 src/a.py")
	}
}

// TestNestedAlternation 嵌套花括号。
func TestNestedAlternation(t *testing.T) {
	p := MustCompile("{a,b{c,d}}.go")
	for _, want := range []string{"a.go", "bc.go", "bd.go"} {
		if !p.Match(want) {
			t.Errorf("应匹配 %q", want)
		}
	}
}

// TestSingleStarDoesNotCrossSlash 单 * 不跨目录，这是 ** 存在的意义。
func TestSingleStarDoesNotCrossSlash(t *testing.T) {
	p := MustCompile("*.go")
	if !p.Match("main.go") {
		t.Error("应匹配 main.go")
	}
	if p.Match("a/b.go") {
		t.Error("单 * 不应跨目录匹配 a/b.go")
	}
}

// TestDoubleStarZeroLayer **/ 必须能匹配零层目录（根目录下的文件）。
func TestDoubleStarZeroLayer(t *testing.T) {
	p := MustCompile("**/*.go")
	for _, want := range []string{"main.go", "a/main.go", "a/b/c/main.go"} {
		if !p.Match(want) {
			t.Errorf("** 应匹配零层与多层，失败: %q", want)
		}
	}
}

// TestDoubleStarNoSlash ** 不带斜杠时跨任意内容。
func TestDoubleStarNoSlash(t *testing.T) {
	p := MustCompile("a/**")
	if !p.Match("a/b/c/d.txt") {
		t.Error("a/** 应匹配任意深层文件")
	}
}

// TestCharacterClass 字符类与取反。
func TestCharacterClass(t *testing.T) {
	p := MustCompile("[abc].go")
	for _, want := range []string{"a.go", "b.go", "c.go"} {
		if !p.Match(want) {
			t.Errorf("应匹配 %q", want)
		}
	}
	if p.Match("d.go") {
		t.Error("不应匹配 d.go")
	}

	neg := MustCompile("[!abc].go")
	if !neg.Match("d.go") {
		t.Error("[!abc] 应匹配 d.go")
	}
	if neg.Match("a.go") {
		t.Error("[!abc] 不应匹配 a.go")
	}

	// [^abc] 与 [!abc] 等价。
	negCaret := MustCompile("[^abc].go")
	if !negCaret.Match("d.go") || negCaret.Match("a.go") {
		t.Error("[^abc] 语义应与 [!abc] 一致")
	}
}

// TestEscape 转义后按字面量处理。
func TestEscape(t *testing.T) {
	p := MustCompile(`a\*b.go`)
	if !p.Match("a*b.go") {
		t.Error(`a\*b.go 应字面匹配 a*b.go`)
	}
	if p.Match("axxb.go") {
		t.Error("转义的 * 不应作为通配符")
	}
}

// TestLiteralBraces 不含逗号的 {x} 视为字面量（避免把正则量词当 glob）。
func TestLiteralBraces(t *testing.T) {
	p := MustCompile("{x}.go")
	if !p.Match("{x}.go") {
		t.Error("不含逗号的花括号应按字面量处理")
	}
}

// TestSyntaxErrorsAreExplicit 语法问题必须报错，绝不静默返回 0 命中。
//
// 这是本包存在的第二动机：区分「模式非法」与「合法但没有匹配」。
func TestSyntaxErrorsAreExplicit(t *testing.T) {
	cases := []string{
		"{a,b",   // 花括号未闭合
		"[abc",   // 方括号未闭合
		"[]",     // 空字符类
		"a[b/c]", // 字符类内出现 /
		`a\`,     // 结尾孤立反斜杠
	}
	for _, pat := range cases {
		if _, err := Compile(pat); err == nil {
			t.Errorf("模式 %q 应报错，实际编译成功", pat)
		} else if !errors.Is(err, ErrUnsupported) {
			t.Errorf("模式 %q 的错误应可用 errors.Is 判定为 ErrUnsupported，得到 %v", pat, err)
		}
	}
}

// TestValidPatternsCompile 常见合法模式不得误报。
func TestValidPatternsCompile(t *testing.T) {
	for _, pat := range []string{
		"*", "**", "**/*", "**/*.go", "src/**/*.ts", "a?c", "*.[ch]", "{a,b}/**/*.go",
	} {
		if _, err := Compile(pat); err != nil {
			t.Errorf("合法模式 %q 编译失败: %v", pat, err)
		}
	}
}

// TestMatchNormalizesDotSlash 调用方常传 ./x.go，匹配前应归一。
func TestMatchNormalizesDotSlash(t *testing.T) {
	p := MustCompile("*.go")
	if !p.Match("./main.go") {
		t.Error("./main.go 应归一后匹配 *.go")
	}
}

// TestNilPatternSafe nil 模式不得 panic。
func TestNilPatternSafe(t *testing.T) {
	var p *Pattern
	if p.Match("a.go") {
		t.Error("nil 模式应返回 false")
	}
}

// TestWindowsCaseInsensitive 仅 Windows 断言大小写不敏感。
func TestWindowsCaseInsensitive(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过平台相关断言")
	}
	p := MustCompile("*.GO")
	src := p.Regexp().String()
	hasFold := strings.HasPrefix(src, "(?i)")
	if hasFold {
		if !p.Match("main.go") {
			t.Error("Windows 下 *.GO 应匹配 main.go")
		}
		return
	}
	// 非 Windows 平台：大小写敏感是正确行为。
	if p.Match("main.go") {
		t.Error("非 Windows 平台大小写应敏感")
	}
}
