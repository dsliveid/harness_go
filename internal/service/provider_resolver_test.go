package service

import (
	"context"
	"encoding/json"
	"testing"

	"harness-go/internal/llm"
	"harness-go/internal/model"
	"harness-go/internal/store"
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

// TestProviderResolverDefaultChain 覆盖「默认模型」解析链（model_ref 为空/"default"）：
// agent.default_model（须命中启用厂商且模型在列表内）→ 首个启用厂商的首个模型 → fallback。
//
// 回归背景：此前 "default" 直接短路到 fake provider，配置了厂商也只会得到回显假回复。
func TestProviderResolverDefaultChain(t *testing.T) {
	ctx := context.Background()
	main, err := store.OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	// Windows：TempDir 清理要求先关闭数据库句柄（后注册先执行，先于目录删除）
	t.Cleanup(func() { _ = main.Close() })

	svc := NewProviderService(ProviderServiceOptions{Main: main})
	p1, err := svc.Create(ctx, model.Provider{Name: "p1", BaseURL: "https://p1.example.com", Models: []string{"m1", "m2"}, Enabled: true})
	if err != nil {
		t.Fatalf("创建厂商失败: %v", err)
	}
	p2, err := svc.Create(ctx, model.Provider{Name: "p2", BaseURL: "https://p2.example.com", Models: []string{"x1"}, Enabled: false})
	if err != nil {
		t.Fatalf("创建停用厂商失败: %v", err)
	}

	fake := llm.NewFakeProvider()
	r := NewProviderResolver(main, nil, nil, fake, "fake")

	setDefault := func(ref string) {
		t.Helper()
		raw, _ := json.Marshal(ref)
		if err := main.Settings().Set(ctx, settingsKeyDefaultModel, raw); err != nil {
			t.Fatalf("写入默认模型失败: %v", err)
		}
	}

	// ① 未设置 agent.default_model → 首个启用厂商的首个模型
	if _, name, err := r.Resolve(ctx, "default"); err != nil || name != "m1" {
		t.Fatalf("无默认设置应解析到首个启用厂商首个模型（裸模型名），得到 (%q, %v)", name, err)
	}

	// ② agent.default_model 以 `<providerID>/<modelID>` 命中 → 精确解析；空 ref 与 "default" 等价
	setDefault(p1.ID + "/m2")
	if _, name, _ := r.Resolve(ctx, ""); name != "m2" {
		t.Fatalf("agent.default_model 命中应解析 m2，得到 %q", name)
	}
	if _, name, _ := r.Resolve(ctx, "default"); name != "m2" {
		t.Fatalf("default 应与空 ref 等价，得到 %q", name)
	}

	// ③ 默认模型以裸模型名命中（无厂商前缀）
	setDefault("m2")
	if _, name, _ := r.Resolve(ctx, ""); name != "m2" {
		t.Fatalf("裸模型名命中应解析 m2，得到 %q", name)
	}

	// ④ 默认模型失效（模型不在列表内）→ 回落首个启用厂商首个模型
	setDefault(p1.ID + "/gone")
	if _, name, _ := r.Resolve(ctx, ""); name != "m1" {
		t.Fatalf("失效默认模型应回落 m1，得到 %q", name)
	}

	// ⑤ 默认模型指向停用厂商 → 回落
	setDefault(p2.ID + "/x1")
	if _, name, _ := r.Resolve(ctx, ""); name != "m1" {
		t.Fatalf("默认模型指向停用厂商应回落 m1，得到 %q", name)
	}

	// ⑥ 全部厂商停用 → 回退 fallback（fake），绝不报错卡死对话
	if _, err := svc.Update(ctx, p1.ID, model.Provider{Name: "p1", Enabled: false}); err != nil {
		t.Fatalf("停用厂商失败: %v", err)
	}
	if _, name, err := r.Resolve(ctx, ""); err != nil || name != "fake" {
		t.Fatalf("无启用厂商应回退 fallback，得到 (%q, %v)", name, err)
	}
}
