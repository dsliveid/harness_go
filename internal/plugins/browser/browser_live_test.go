package browser

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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

// livePage 起本地 HTML 页（含按钮 / 输入框 / 超高内容），供交互类 live 测试使用。
func livePage(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body>
<div style="height:4000px"; id="filler">tall</div>
<button id="btn" onclick="document.getElementById('out').textContent='clicked'">go</button>
<input id="name" type="text">
<div id="out">idle</div>
</body></html>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestBrowserInteractLive 真实浏览器验证 click / type / scroll / screenshot 全链路。
func TestBrowserInteractLive(t *testing.T) {
	h := plugintest.New(t)
	p := &plugin{Host: h}
	if p.probe() == "" {
		t.Skip("本机无 Chromium 系浏览器，跳过交互能力 live 测试")
	}
	page := livePage(t)

	// click：命中按钮
	res, err := invokeClick(t.Context(), p, &toolapi.Call{
		Args: []byte(`{"url":"` + page + `","selector":"#btn","wait_ms":500}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("click 失败: %q", res.Content)
	}

	// click：选择器未命中 → 显式错误且回报选择器
	res, _ = invokeClick(t.Context(), p, &toolapi.Call{
		Args: []byte(`{"url":"` + page + `","selector":"#nope","wait_ms":300}`),
	})
	if !res.IsError || !strings.Contains(res.Content, "#nope") {
		t.Fatalf("未命中必须显式报错并列出选择器: %q", res.Content)
	}

	// type：写入输入框并回显值
	res, _ = invokeType(t.Context(), p, &toolapi.Call{
		Args: []byte(`{"url":"` + page + `","selector":"#name","text":"hello","wait_ms":300}`),
	})
	if res.IsError || !strings.Contains(res.Content, "hello") {
		t.Fatalf("type 失败: %q", res.Content)
	}

	// scroll：向下滚动后 scrollY 应 > 0
	res, _ = invokeScroll(t.Context(), p, &toolapi.Call{
		Args: []byte(`{"url":"` + page + `","direction":"down","pixels":800,"wait_ms":300}`),
	})
	if res.IsError || !strings.Contains(res.Content, "scrollY") {
		t.Fatalf("scroll 失败: %q", res.Content)
	}
	if z := scrollYOf(t, res.Content); z <= 0 {
		t.Fatalf("向下滚动后 scrollY 应 > 0: %q", res.Content)
	}

	// screenshot：落盘到工作区
	rel := "shots/live.png"
	res, _ = invokeScreenshot(t.Context(), p, &toolapi.Call{
		Args: []byte(`{"url":"` + page + `","path":"` + rel + `","wait_ms":300}`),
	})
	if res.IsError {
		t.Fatalf("screenshot 失败: %q", res.Content)
	}
	abs, err := h.Workspace().Resolve(rel)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("截图未落盘: %v", err)
	}
	if len(data) < 8 || string(data[1:4]) != "PNG" {
		t.Fatalf("落盘文件不是 PNG（%d 字节）", len(data))
	}
}

// scrollYOf 从 scroll 结果文本里取 scrollY 数值。
func scrollYOf(t *testing.T, content string) float64 {
	t.Helper()
	i := strings.Index(content, "scrollY=")
	if i < 0 {
		t.Fatalf("结果缺少 scrollY: %q", content)
	}
	rest := content[i+len("scrollY="):]
	if j := strings.IndexAny(rest, "（( \n"); j >= 0 {
		rest = rest[:j]
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
	if err != nil {
		t.Fatalf("解析 scrollY 失败: %v（%q）", err, content)
	}
	return v
}
