package observ

import (
	"net/url"
	"strings"
)

// redactAllowed 是 access log 中允许保留的 query 参数白名单。
//
// 铁律（docs/开发实施计划.md 附录 A）：SSO 免登入口的 pwd / sign 永不落日志；
// user_name 打码保留，便于排障时确认"是谁在尝试登录"。
var redactAllowed = map[string]bool{
	"user_name": true, // 打码
	"redirect":  true, // 保留（不含凭据）
}

// RedactQuery 对 URL 原始 query 做白名单脱敏。
//
// 规则：
//   - 非白名单键**整体丢弃**（连键名都不输出，避免"某参数曾出现"这一信息泄露）
//   - user_name 打码为 a***n 形式
//   - 无法解析时返回空串（宁可丢信息，也不泄露原文）
func RedactQuery(rawQuery string) string {
	rawQuery = strings.TrimSpace(rawQuery)
	if rawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return ""
	}
	parts := make([]string, 0, len(values))
	for key, vals := range values {
		if !redactAllowed[key] || len(vals) == 0 {
			continue
		}
		v := vals[0]
		if key == "user_name" {
			// 打码值只含字母数字与 *（或原文首尾字符），直接拼接以便日志可读
			parts = append(parts, key+"="+MaskName(v))
			continue
		}
		parts = append(parts, key+"="+url.QueryEscape(v))
	}
	// url.Values 遍历无序，这里用简单稳定顺序（白名单很小）
	return strings.Join(sortKeys(parts), "&")
}

// MaskName 把账号打码：admin → a***n，短账号全掩。
func MaskName(s string) string {
	r := []rune(s)
	switch {
	case len(r) == 0:
		return ""
	case len(r) <= 2:
		return "***"
	default:
		return string(r[0]) + "***" + string(r[len(r)-1])
	}
}

// sortKeys 对 "k=v" 片段按 key 排序，保证日志可对比。
func sortKeys(parts []string) []string {
	if len(parts) < 2 {
		return parts
	}
	// 插入排序（元素极少，避免引入 sort 包的额外开销）
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && parts[j] < parts[j-1]; j-- {
			parts[j], parts[j-1] = parts[j-1], parts[j]
		}
	}
	return parts
}
