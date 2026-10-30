package service

import (
	"context"
	"testing"

	"harness-go/internal/llm"
)

// TestSplitModelRef 覆盖 `<providerID>/<modelID>` 的保守拆分规则。
//
// 关键约束：真实模型名常自带斜杠（openai/gpt-4o-mini、deepseek/deepseek-chat），
// 不能一律按第一个 `/` 拆分，否则会把 `openai` 误认成厂商 ID。
func TestSplitModelRef(t *testing.T) {
	cases := []struct {
		ref    string
		wantOK bool
		wantID string
		wantMD string
	}{
		{"pv_1700000000000/gpt-4o-mini", true, "pv_1700000000000", "gpt-4o-mini"},
		{"0123456789abcdef0123456789abcdef/deepseek-chat", true, "0123456789abcdef0123456789abcdef", "deepseek-chat"},
		{"openai/gpt-4o-mini", false, "", ""},
		{"deepseek/deepseek-chat", false, "", ""},
		{"plain-model", false, "", ""},
		{"", false, "", ""},
		{"pv_x/", false, "", ""},
		{"/model", false, "", ""},
	}
	for _, c := range cases {
		id, md, ok := splitModelRef(c.ref)
		if ok != c.wantOK || id != c.wantID || md != c.wantMD {
			t.Fatalf("splitModelRef(%q) = (%q, %q, %v)，期望 (%q, %q, %v)",
				c.ref, id, md, ok, c.wantID, c.wantMD, c.wantOK)
		}
	}
}

// TestIsHexID 覆盖厂商 ID 判定（主库生成的 32 位十六进制串）。
func TestIsHexID(t *testing.T) {
	if !isHexID("0123456789abcdef0123456789abcdef") {
		t.Fatal("32 位十六进制应判定为 ID")
	}
	if isHexID("pv_1700000000000") {
		t.Fatal("pv_ 前缀不是十六进制 ID")
	}
	if isHexID("abc") {
		t.Fatal("过短字符串不应判定为 ID")
	}
	if isHexID("0123456789ABCDEF0123456789ABCDEF") {
		t.Fatal("大写十六进制不匹配（主库生成小写）")
	}
}

// TestProviderResolverFallback 未配置厂商/未命中时必须回退 fallback，绝不能返回错误卡死对话。
func TestProviderResolverFallback(t *testing.T) {
	fake := llm.NewFakeProvider()
	r := NewProviderResolver(nil, nil, nil, fake, "fake")

	for _, ref := range []string{"", "default", "openai/gpt-4o-mini"} {
		// main 为 nil（模拟主库未注入）→ 必须回退而不是 panic
		p, name, err := r.Resolve(context.Background(), ref)
		if err != nil {
			t.Fatalf("Resolve(%q) 回退失败: %v", ref, err)
		}
		if p == nil || name != "fake" {
			t.Fatalf("Resolve(%q) 应返回 fallback，得到 (%v, %q)", ref, p, name)
		}
	}
}

// TestProviderResolverNoFallback 显式未提供 fallback 时应给出明确错误（不静默成功）。
func TestProviderResolverNoFallback(t *testing.T) {
	r := NewProviderResolver(nil, nil, nil, nil, "")
	if _, _, err := r.Resolve(context.Background(), ""); err == nil {
		t.Fatal("无 fallback 时应返回错误")
	}
}
