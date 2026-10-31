package browser

import (
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

// TestBrowserTextLive 真实无头渲染（本机有 Chromium 系浏览器时执行；
// 无浏览器则跳过——探测降级已由 browser_test.go 覆盖）。
func TestBrowserTextLive(t *testing.T) {
	h := plugintest.New(t)
	p := &plugin{Host: h}
	if p.probe() == "" {
		t.Skip("本机无 Chromium 系浏览器，跳过真实渲染测试")
	}
	// example.com 静态页，验证 CDP 全链路（/json/new → WS → evaluate）
	res, err := invokeText(t.Context(), p, &toolapi.Call{
		Args: []byte(`{"url":"https://example.com","wait_ms":2000}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("渲染失败: %q", res.Content)
	}
	if !strings.Contains(res.Content, "documentation examples") {
		t.Fatalf("正文提取错误: %q", res.Content)
	}
}
