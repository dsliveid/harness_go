package observ

import (
	"regexp"
	"strings"
)

// 报文脱敏（落盘前执行）。
//
// 为什么必须做：日志要记录模型入参/出参的原文，而用户经常把凭据直接粘进对话
// （"帮我看看这个 key: sk-xxx"）。现有 RedactQuery 只覆盖 URL query 白名单，
// **报文 body 完全没有覆盖**——那才是凭据真正会出现的地方。
//
// 设计取向：宁可过度脱敏，也不漏。误杀只会让日志少一段正文，漏杀是安全事故。
//
// ⚠️ 性能红线（实测教训）：本函数跑在**每一次** LLM 调用的写入热路径上。
// 朴素的「无脑跑全部正则」在 40KB 报文上要 5.2ms，直接把 preview 档吞吐从
// 8500 行/秒 拖到 154 行/秒（55 倍劣化）。因此采用两级策略：
//  1. 先用**极廉价的子串预筛**（strings.Contains，Go 的 SIMD 实现）判断是否可能命中；
//  2. 仅对预筛命中的规则才执行正则。
// 真实报文里凭据极少出现，绝大多数调用只付 6 次 Contains 的成本。

// redactRule 一条脱敏规则。
type redactRule struct {
	name string
	// hints 廉价预筛关键字（小写）。任一命中才跑正则；为空表示必须跑。
	hints []string
	re    *regexp.Regexp
	repl  string
}

// redactRules 脱敏规则（顺序即执行顺序：先长后短，避免被短规则先吃掉一部分）。
var redactRules = []redactRule{
	// PEM 私钥块（必须最先整体吃掉，否则会被 key= 规则先截断）
	{
		name:  "private_key",
		hints: []string{"-----begin"},
		re:    regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
		repl:  "***PRIVATE_KEY***",
	},
	// OpenAI / Anthropic 风格密钥（sk-xxx / sk-proj-xxx / sk-ant-xxx）
	{
		name:  "sk_key",
		hints: []string{"sk-"},
		re:    regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
		repl:  "***",
	},
	// Authorization: Bearer <token>
	{
		name:  "bearer",
		hints: []string{"bearer"},
		re:    regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._\-+/=]{8,}`),
		repl:  "${1}***",
	},
	// AWS 风格 Access Key
	{
		name:  "aws_ak",
		hints: []string{"akia"},
		re:    regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		repl:  "***",
	},
	// 通用 password / passwd / pwd 赋值（JSON 与 query 两种形态都覆盖）
	{
		name:  "password",
		hints: []string{"password", "passwd", "pwd"},
		re:    regexp.MustCompile(`(?i)((?:password|passwd|pwd)["']?\s*[:=]\s*)["']?[^\s"',;}\]]+`),
		repl:  "${1}***",
	},
	// api_key / apikey / api-key / access_token / secret 赋值
	{
		name:  "api_key",
		hints: []string{"api_key", "apikey", "api-key", "access_token", "accesstoken", "secret"},
		re:    regexp.MustCompile(`(?i)((?:api[_-]?key|access[_-]?token|secret[_-]?key|secret)["']?\s*[:=]\s*)["']?[^\s"',;}\]]+`),
		repl:  "${1}***",
	},
}

// shouldRun 判断某条规则是否可能命中（廉价预筛）。
func shouldRun(r redactRule, lower string) bool {
	if len(r.hints) == 0 {
		return true
	}
	for _, h := range r.hints {
		if strings.Contains(lower, h) {
			return true
		}
	}
	return false
}

// RedactPayload 对报文文本做凭据脱敏，返回脱敏后的文本。
//
// 幂等：对已脱敏文本再次调用不会进一步改变内容（替换值 `***` 本身不匹配任何规则）。
func RedactPayload(s string) string {
	if s == "" {
		return s
	}
	// 一次 ToLower 供全部规则复用（比每条规则各转一次省 6 倍开销）。
	// 注意：只用于预筛判定，实际替换仍用原串，避免改变大小写。
	lower := strings.ToLower(s)
	out := s
	for _, r := range redactRules {
		if !shouldRun(r, lower) {
			continue
		}
		if r.re.MatchString(out) {
			out = r.re.ReplaceAllString(out, r.repl)
		}
	}
	return out
}

// RedactPayloadBytes 对字节切片做同样脱敏（Blob 写入前调用）。
func RedactPayloadBytes(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	return []byte(RedactPayload(string(b)))
}

// ContainsSecret 判断文本中是否疑似含凭据（用于日志告警，不用于脱敏）。
func ContainsSecret(s string) bool {
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	for _, r := range redactRules {
		if !shouldRun(r, lower) {
			continue
		}
		if r.re.MatchString(s) {
			return true
		}
	}
	return false
}
