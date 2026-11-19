// Package globsyntax 把 glob 模式编译为正则表达式。
//
// 为什么需要独立成包（而不是在 fs 工具里用 filepath.Match）：
//
//	glob 是一门独立的小语言，语法元素包括 ** / * / ? / {a,b} / [abc] / 转义。
//	标准库 filepath.Match 只支持 * / ? / [abc]，** 与 {a,b} 都不支持。此前
//	fs.invokeGlob 直接调 filepath.Match 并自行展开 **，结果是把「语法不支持」
//	静默退化成「文件不存在」——模型拿到「无匹配文件」后会误判项目里没有代码，
//	这是所有误导类型里最危险的一种（真实事故：会话 9ee34e00 首轮即中招，
//	传 **/*.{go,py,js,ts,json,yaml,yml,toml} 得到「无匹配文件」）。
//
// 本包的核心契约（务必遵守）：
//
//  1. 语法问题一律编译期报错，绝不在匹配阶段静默返回 0 命中。
//     调用方必须能区分「模式非法」与「合法但没有匹配」。
//  2. 匹配以 / 为分隔符，** 跨目录、* 不跨目录（与 gitignore 语义一致）。
//  3. 路径统一用 / 分隔，Windows 的反斜杠由调用方先归一化。
package globsyntax

import (
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strings"
)

// ErrUnsupported 表示模式包含无法翻译的语法。
//
// 调用方应把它包装成「错误结果」返回给模型，并**明确告知不支持哪种写法**，
// 而不是回「无匹配文件」。二者对模型的行为引导完全相反。
var ErrUnsupported = errors.New("globsyntax: 模式包含不支持的语法")

// Pattern 是编译后的 glob 模式。
type Pattern struct {
	// Raw 原始模式（日志与报错展示用）。
	Raw string
	// re 编译后的等价正则。
	re *regexp.Regexp
}

// Compile 编译 glob 模式。
//
// 返回的 error 一定是 ErrUnsupported 或 *SyntaxError，绝不会在编译成功后
// 再于匹配阶段报错——匹配是全函数的（total），不存在中途失败。
func Compile(pattern string) (*Pattern, error) {
	src, err := translate(pattern)
	if err != nil {
		return nil, err
	}
	// Windows 文件系统大小写不敏感，模式匹配必须跟随，否则 *.GO 匹配不到 a.go。
	// 这与老版本 harness_mini 的 glob_to_regex 前缀 (?i) 口径一致。
	if runtime.GOOS == "windows" {
		src = "(?i)" + src
	}
	re, err := regexp.Compile(src)
	if err != nil {
		// 翻译器生成的字符串理论上一定合法；走到这里说明翻译器有 bug，
		// 属于程序内部错误而非用户模式错误，必须暴露出来而不是吞掉。
		return nil, fmt.Errorf("globsyntax: 内部错误（翻译结果无法编译）: %w", err)
	}
	return &Pattern{Raw: pattern, re: re}, nil
}

// MustCompile 仅供测试与已知合法常量使用。
func MustCompile(pattern string) *Pattern {
	p, err := Compile(pattern)
	if err != nil {
		panic(err)
	}
	return p
}

// Match 判断**相对路径**是否匹配。path 必须使用 / 分隔。
func (p *Pattern) Match(path string) bool {
	if p == nil || p.re == nil {
		return false
	}
	return p.re.MatchString(strings.TrimPrefix(path, "./"))
}

// Regexp 返回底层正则（调试与测试用，调用方不应改它）。
func (p *Pattern) Regexp() *regexp.Regexp { return p.re }

// translate 把 glob 翻译为正则源串（不带大小写与锚定前缀）。
//
// 逐个字符扫描而不是正则替换，保证「转义状态」被正确携带——
// 用字符串替换实现转义是这类翻译器最常见的出错点（\\* 会被当成 * 处理）。
func translate(pattern string) (string, error) {
	var b strings.Builder
	b.WriteString("^")

	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		switch c := runes[i]; c {
		case '\\':
			// 转义：下一个字符按字面量处理。
			if i+1 >= len(runes) {
				return "", fmt.Errorf("%w: 结尾的反斜杠没有要转义的字符（模式 %q）", ErrUnsupported, pattern)
			}
			i++
			b.WriteString(regexp.QuoteMeta(string(runes[i])))

		case '*':
			if i+1 < len(runes) && runes[i+1] == '*' {
				i++ // 消费第二个 *
				// **/ 视为「零层或多层目录」，即可以匹配空前缀。
				// 这是与 filepath.Match 的关键差异：** 必须能吃掉分隔符。
				if i+1 < len(runes) && runes[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
				continue
			}
			// 单 * 不跨目录。
			b.WriteString("[^/]*")

		case '?':
			b.WriteString("[^/]")

		case '{':
			alt, next, err := translateAlternation(runes, i, pattern)
			if err != nil {
				return "", err
			}
			b.WriteString(alt)
			i = next

		case '[':
			cls, next, err := translateClass(runes, i, pattern)
			if err != nil {
				return "", err
			}
			b.WriteString(cls)
			i = next

		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}

	b.WriteString("$")
	return b.String(), nil
}

// translateAlternation 处理 {a,b,c}。
//
// 必须显式支持：这是本包存在的主要原因。允许嵌套（{a,{b,c}}）由递归天然支持；
// 不含逗号的 {x} 视为字面量（与 bash 行为一致，也避免把正则量词误当 glob）。
func translateAlternation(runes []rune, start int, pattern string) (string, int, error) {
	depth := 0
	closeIdx := -1
	for i := start; i < len(runes); i++ {
		switch runes[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				closeIdx = i
			}
		}
		if closeIdx >= 0 {
			break
		}
	}
	if closeIdx < 0 {
		return "", 0, fmt.Errorf("%w: 花括号未闭合（模式 %q）", ErrUnsupported, pattern)
	}

	inner := string(runes[start+1 : closeIdx])
	parts, err := splitAlternation(inner)
	if err != nil {
		return "", 0, err
	}
	// 没有逗号 → 不是备选结构，按字面量 {x} 处理。
	if len(parts) == 1 {
		return regexp.QuoteMeta("{" + inner + "}"), closeIdx, nil
	}

	var out []string
	for _, part := range parts {
		sub, err := translate(part)
		if err != nil {
			return "", 0, err
		}
		// translate 产出带 ^$ 锚定，嵌套时必须剥掉。
		sub = strings.TrimSuffix(strings.TrimPrefix(sub, "^"), "$")
		out = append(out, sub)
	}
	return "(?:" + strings.Join(out, "|") + ")", closeIdx, nil
}

// splitAlternation 按顶层逗号切分（忽略嵌套花括号与方括号内的逗号）。
func splitAlternation(inner string) ([]string, error) {
	var parts []string
	var cur strings.Builder
	depthBrace, depthBracket := 0, 0
	escaped := false
	for _, c := range inner {
		switch {
		case escaped:
			cur.WriteRune(c)
			escaped = false
			continue
		case c == '\\':
			cur.WriteRune(c)
			escaped = true
			continue
		}
		switch c {
		case '{':
			depthBrace++
		case '}':
			depthBrace--
		case '[':
			depthBracket++
		case ']':
			if depthBracket > 0 {
				depthBracket--
			}
		case ',':
			if depthBrace == 0 && depthBracket == 0 {
				parts = append(parts, cur.String())
				cur.Reset()
				continue
			}
		}
		if depthBrace < 0 || depthBracket < 0 {
			return nil, fmt.Errorf("%w: 花括号/方括号不配对", ErrUnsupported)
		}
		cur.WriteRune(c)
	}
	parts = append(parts, cur.String())
	return parts, nil
}

// translateClass 处理 [abc] / [a-z] / [!abc] / [^abc]。
//
// 关键点：字符类内部 * 与 ? 是字面量（glob 与正则在此一致），而 / 绝不允许
// 出现在类里——否则 [a/b] 会跨越目录分隔符，破坏「* 不跨目录」的不变量。
func translateClass(runes []rune, start int, pattern string) (string, int, error) {
	closeIdx := -1
	for i := start + 1; i < len(runes); i++ {
		if runes[i] == ']' {
			closeIdx = i
			break
		}
	}
	if closeIdx < 0 {
		return "", 0, fmt.Errorf("%w: 方括号未闭合（模式 %q）", ErrUnsupported, pattern)
	}

	inner := runes[start+1 : closeIdx]
	if len(inner) == 0 {
		return "", 0, fmt.Errorf("%w: 空的方括号 []（模式 %q）", ErrUnsupported, pattern)
	}

	var b strings.Builder
	b.WriteString("[")
	for i, c := range inner {
		// [!...] 与 [^...] 都表示取反（glob 惯例用 !，正则惯例用 ^，两者都接受）。
		if i == 0 && (c == '!' || c == '^') {
			b.WriteString("^")
			continue
		}
		if c == '/' {
			return "", 0, fmt.Errorf("%w: 字符类内不能出现 /（模式 %q）", ErrUnsupported, pattern)
		}
		if c == '\\' || c == ']' || c == '^' {
			b.WriteString("\\")
		}
		b.WriteRune(c)
	}
	b.WriteString("]")
	return b.String(), closeIdx, nil
}
