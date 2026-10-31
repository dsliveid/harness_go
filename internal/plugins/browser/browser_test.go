package browser

import (
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

// TestBrowserProbeDegradesExplicitly 探测结果必须显式：有路径或可读缺失说明（绝不静默假装）。
func TestBrowserProbeDegradesExplicitly(t *testing.T) {
	h := plugintest.New(t)
	p := &plugin{Host: h}
	res, err := invokeProbe(t.Context(), h, &toolapi.Call{})
	if err != nil {
		t.Fatal(err)
	}
	bin := p.probe()
	if bin != "" {
		if !strings.Contains(res.Content, bin) {
			t.Fatalf("探测结果应含路径: %q", res.Content)
		}
		if health := p.Health(); health.Status != toolapi.HealthAvailable {
			t.Fatalf("有浏览器时 Health 应 available: %+v", health)
		}
		return
	}
	if !res.IsError || !strings.Contains(res.Content, "不可用") {
		t.Fatalf("缺失时必须显式报不可用: %q", res.Content)
	}
	// Health 同口径：缺失 → unavailable + 可读原因
	if health := p.Health(); health.Status != toolapi.HealthUnavailable || !strings.Contains(health.Message, "Chromium") {
		t.Fatalf("缺失时 Health 应 unavailable: %+v", health)
	}
}

// TestBrowserTextWithoutChromium 无浏览器时 browser_text 显式拒绝。
func TestBrowserTextWithoutChromium(t *testing.T) {
	h := plugintest.New(t)
	p := &plugin{Host: h}
	p.mu.Lock()
	p.probed = true // 强制「未找到」分支（探测缓存置空态）
	p.bin = ""
	p.mu.Unlock()
	// 注意：必须把 p 自身作为 Host 传入（hostOf 才会用同一探测缓存）
	res, _ := invokeText(t.Context(), p, &toolapi.Call{Args: []byte(`{"url":"https://example.com"}`)})
	if !res.IsError || !strings.Contains(res.Content, "不可用") {
		t.Fatalf("无 Chromium 必须显式拒绝: %q", res.Content)
	}
	// 非法 URL
	res, _ = invokeText(t.Context(), h, &toolapi.Call{Args: []byte(`{"url":"ftp://x"}`)})
	if !res.IsError || !strings.Contains(res.Content, "URL 非法") {
		t.Fatalf("非法 URL 必须拒绝: %q", res.Content)
	}
}
