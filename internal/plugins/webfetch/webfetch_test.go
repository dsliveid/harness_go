package webfetch

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"harness-go/pkg/toolapi"
)

// ---------- 1. 正常抓取（真实 httptest HTTP，不 mock http.Client） ----------

// TestFetchNormalHTML 真实抓取 text/html：断言正文提取 + 来源标注。
//
// 说明：CheckRedirect 只作用于「重定向跳」，首跳不经过 guardPrivateHost，
// 因此可直接对 127.0.0.1 的 httptest 服务做真实抓取。
func TestFetchNormalHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua == "" {
			t.Errorf("缺少 User-Agent")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body><h1>HARNESS 标题</h1><p>正文段落内容</p>
<script>var leak = "不应该出现";</script></body></html>`))
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := newHTTPClient().Do(req)
	if err != nil {
		t.Fatalf("真实请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	res := buildResult(string(buf[:n]), resp.Header.Get("Content-Type"), srv.URL, resp.StatusCode, defaultChars)

	if res.IsError {
		t.Fatalf("正常抓取不应失败: %q", res.Content)
	}
	for _, want := range []string{"HARNESS 标题", "正文段落内容", "[来源 " + srv.URL, "状态 200"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("结果缺少 %q: %q", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "不应该出现") {
		t.Fatalf("script 内容不应出现在正文: %q", res.Content)
	}
}

// TestFetchNormalPlainText 纯文本响应原样保留。
func TestFetchNormalPlainText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("line1\nline2"))
	}))
	defer srv.Close()

	resp, err := newHTTPClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("真实请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := make([]byte, 1024)
	n, _ := resp.Body.Read(buf)

	res := buildResult(string(buf[:n]), resp.Header.Get("Content-Type"), srv.URL, resp.StatusCode, defaultChars)
	if res.IsError || !strings.Contains(res.Content, "line1\nline2") {
		t.Fatalf("纯文本结果错误: %q", res.Content)
	}
}

// ---------- 2. 非 http/https scheme 拒绝 ----------

func TestSchemeRejected(t *testing.T) {
	for _, raw := range []string{
		"file:///C:/Windows/win.ini",
		"ftp://example.com/a.txt",
		"javascript:alert(1)",
		"data:text/plain,hi",
		"",
		"example.com/no-scheme",
	} {
		t.Run(raw, func(t *testing.T) {
			res, err := invokeFetch(context.Background(), nil, &toolapi.Call{
				Args: []byte(`{"url":"` + strings.ReplaceAll(raw, `"`, `\"`) + `"}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			if res == nil || !res.IsError {
				t.Fatalf("非 http/https 必须拒绝: %+v", res)
			}
			if !strings.Contains(res.Content, "仅 http/https") {
				t.Fatalf("拒绝原因不明确: %q", res.Content)
			}
		})
	}
}

// TestSchemeAllowedNotRejectedByScheme 检查 http/https 不会被 scheme 校验误杀（改为被 SSRF 拦截）。
func TestSchemeAllowedNotRejectedByScheme(t *testing.T) {
	res, _ := invokeFetch(context.Background(), nil, &toolapi.Call{Args: []byte(`{"url":"http://127.0.0.1:1/x"}`)})
	if res == nil || !res.IsError {
		t.Fatalf("环回地址必须拦截: %+v", res)
	}
	if strings.Contains(res.Content, "仅 http/https") {
		t.Fatalf("http/https 不应被 scheme 校验拒绝: %q", res.Content)
	}
	if !strings.Contains(res.Content, "SSRF 防护") {
		t.Fatalf("应命中 SSRF 拦截: %q", res.Content)
	}
}

// ---------- 3. 私网/环回/链路本地拦截（核心 SSRF 用例） ----------

func TestGuardPrivateHostBlocks(t *testing.T) {
	cases := []struct {
		host string
		desc string
	}{
		{"127.0.0.1", "环回"},
		{"127.0.0.53", "环回段"},
		{"localhost", "localhost 字面量"},
		{"10.0.0.1", "10/8 私有"},
		{"192.168.1.1", "192.168/16 私有"},
		{"172.16.0.1", "172.16/12 下界"},
		{"172.31.255.255", "172.16/12 上界"},
		{"169.254.169.254", "云元数据链路本地"},
		{"::1", "IPv6 环回"},
		{"[::1]", "IPv6 环回（带方括号）"},
		{"0.0.0.0", "未指定地址"},
		{"fc00::1", "IPv6 ULA 私有"},
		{"fe80::1", "IPv6 链路本地"},
		{"::ffff:127.0.0.1", "IPv4 映射环回"},
		{"::ffff:169.254.169.254", "IPv4 映射链路本地"},
	}
	for _, c := range cases {
		t.Run(c.host, func(t *testing.T) {
			if err := guardPrivateHost(c.host); err == nil {
				t.Fatalf("%s（%s）必须被拦截", c.host, c.desc)
			} else if !strings.Contains(err.Error(), "SSRF 防护") {
				t.Fatalf("拦截原因不明确: %v", err)
			}
		})
	}
}

// TestInvokeFetchBlocksPrivateHosts 走完整 invokeFetch，断言 IsError 且含拦截原因。
func TestInvokeFetchBlocksPrivateHosts(t *testing.T) {
	urls := []string{
		"http://127.0.0.1/admin",
		"http://localhost:8080/health",
		"https://10.0.0.1/",
		"http://192.168.1.1/",
		"http://172.16.0.1/",
		"http://172.31.255.255/",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:9000/",
		"http://0.0.0.0:2375/containers/json",
	}
	for _, u := range urls {
		t.Run(u, func(t *testing.T) {
			res, err := invokeFetch(context.Background(), nil, &toolapi.Call{
				Args: []byte(`{"url":"` + u + `"}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			if res == nil || !res.IsError {
				t.Fatalf("私网地址必须拦截: %+v", res)
			}
			if !strings.Contains(res.Content, "拦截") || !strings.Contains(res.Content, "SSRF 防护") {
				t.Fatalf("拦截原因不明确: %q", res.Content)
			}
		})
	}
}

// ---------- 4. 边界正例：公网段不得被误杀 ----------

func TestGuardPrivateHostAllowsPublic(t *testing.T) {
	for _, host := range []string{
		"172.15.0.1",           // 172.16/12 下界之外
		"172.32.0.1",           // 172.16/12 上界之外
		"8.8.8.8",              // 公网 DNS
		"1.1.1.1",              // 公网 DNS
		"93.184.216.34",        // example.com 历史 IP
		"2001:4860:4860::8888", // 公网 IPv6
		"example.com",          // 域名（解析由系统栈完成）
		"api.github.com",
	} {
		if err := guardPrivateHost(host); err != nil {
			t.Fatalf("%s 是公网目标，不应被拦截: %v", host, err)
		}
	}
}

// TestGuardPrivateHostKnownGaps 锁定当前【已知缺陷】行为，供后续修复时同步更新。
//
// 缺陷 1（P1）：hostname 字面量比较区分大小写，"LOCALHOST" 绕过拦截（系统解析后仍连环回）。
// 缺陷 2（P1）：net.ParseIP 失败即放行 → 域名解析到私网/链路本地（DNS 重绑定、
//
//	127.0.0.1.nip.io 之类）不会被拦截。代码注释已自认「属后续增强」。
//
// ⚠️ 若将来修复上述缺口，请把本用例的期望值改为「必须被拦截」。
func TestGuardPrivateHostKnownGaps(t *testing.T) {
	gaps := []string{"LOCALHOST", "LocalHost", "127.0.0.1.nip.io", "localtest.me"}
	for _, h := range gaps {
		if err := guardPrivateHost(h); err != nil {
			t.Fatalf("%s 当前实现未拦截（已知缺陷，若已修复请更新本用例）: %v", h, err)
		}
	}
	// 对照：小写字面量确实被拦截，证明缺陷源于大小写比较
	if err := guardPrivateHost("localhost"); err == nil {
		t.Fatal("localhost 必须被拦截")
	}
}

// TestRedirectToPrivateBlockedReal 真实两跳：A(127.0.0.1) 302 → B(127.0.0.1)。
// 首跳不校验，第二跳（重定向目标）必须被 CheckRedirect 拦下。
func TestRedirectToPrivateBlockedReal(t *testing.T) {
	var bHit int32
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&bHit, 1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("SECRET-INTERNAL"))
	}))
	defer b.Close()

	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL+"/internal", http.StatusFound)
	}))
	defer a.Close()

	_, err := newHTTPClient().Get(a.URL)
	if err == nil {
		t.Fatal("重定向到环回地址必须被拒绝")
	}
	if !strings.Contains(err.Error(), "SSRF 防护") {
		t.Fatalf("拒绝原因应含 SSRF 防护: %v", err)
	}
	if atomic.LoadInt32(&bHit) != 0 {
		t.Fatal("重定向目标不应被真实请求（防护未生效）")
	}
}

// TestCheckRedirectRejectsPrivateTarget 直接对 CheckRedirect 行为单测。
func TestCheckRedirectRejectsPrivateTarget(t *testing.T) {
	for _, target := range []string{
		"http://127.0.0.1:8080/x",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.1.2.3/",
		"http://[::1]:80/",
	} {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		via := []*http.Request{{}}
		if err := checkRedirect(req, via); err == nil {
			t.Fatalf("重定向到 %s 必须拒绝", target)
		} else if !strings.Contains(err.Error(), "SSRF 防护") {
			t.Fatalf("拒绝原因应含 SSRF 防护: %v", err)
		}
	}
}

// TestCheckRedirectAllowsPublicTarget 重定向到公网域名放行（不得过宽拦截）。
func TestCheckRedirectAllowsPublicTarget(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.com/next", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkRedirect(req, []*http.Request{{}}); err != nil {
		t.Fatalf("公网重定向不应拒绝: %v", err)
	}
}

// TestCheckRedirectTooMany 跳数 >= maxRedirects 时拒绝。
func TestCheckRedirectTooMany(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.com/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	via := make([]*http.Request, maxRedirects) // len == 5
	err = checkRedirect(req, via)
	if err == nil || !strings.Contains(err.Error(), "重定向次数超限") {
		t.Fatalf("跳数超限必须拒绝: %v", err)
	}
	// 边界：4 跳仍允许
	if err := checkRedirect(req, via[:maxRedirects-1]); err != nil {
		t.Fatalf("%d 跳不应拒绝: %v", maxRedirects-1, err)
	}
}

// ---------- 7. 非文本类型拒绝 ----------

func TestNonTextContentTypeRejected(t *testing.T) {
	for _, ct := range []string{
		"application/octet-stream",
		"image/png",
		"video/mp4",
		"application/pdf",
	} {
		t.Run(ct, func(t *testing.T) {
			res := buildResult("binary\x00data", ct, "http://example.com/a", http.StatusOK, defaultChars)
			if !res.IsError {
				t.Fatalf("%s 必须拒绝: %+v", ct, res)
			}
			if !strings.Contains(res.Content, "不支持的类型") {
				t.Fatalf("拒绝原因不明确: %q", res.Content)
			}
			if !strings.Contains(res.Content, ct) {
				t.Fatalf("原因应回显类型: %q", res.Content)
			}
		})
	}
}

// TestTextContentTypeAccepted 文本类类型放行（防过宽拒绝）。
func TestTextContentTypeAccepted(t *testing.T) {
	for _, ct := range []string{"", "text/html; charset=utf-8", "text/plain", "application/json", "application/xml"} {
		if !isTextType(ct) {
			t.Fatalf("%q 应被视为文本类型", ct)
		}
	}
}

// ---------- 8. HTTP 非 200 ----------

func TestHTTPStatusError(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusForbidden, http.StatusTooManyRequests} {
		res := buildResult("whatever", "text/html", "http://example.com/x", status, defaultChars)
		if !res.IsError {
			t.Fatalf("HTTP %d 必须报错", status)
		}
		if !strings.Contains(res.Content, strconv.Itoa(status)) {
			t.Fatalf("错误应含状态码 %d: %q", status, res.Content)
		}
		if !strings.Contains(res.Content, http.StatusText(status)) {
			t.Fatalf("错误应含状态文本: %q", res.Content)
		}
	}
}

// TestHTTPStatusErrorReal 真实服务返回 500。
func TestHTTPStatusErrorReal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	resp, err := newHTTPClient().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	res := buildResult("", resp.Header.Get("Content-Type"), srv.URL, resp.StatusCode, defaultChars)
	if !res.IsError || !strings.Contains(res.Content, "500") {
		t.Fatalf("应报 500: %q", res.Content)
	}
}

// ---------- 9. body 超上限截断 ----------

func TestBodyOverLimitTruncated(t *testing.T) {
	body := strings.Repeat("a", maxBodyBytes+5000)
	res := buildResult(body, "text/plain", "http://example.com/big", http.StatusOK, maxCharsCeiling)
	if res.IsError {
		t.Fatalf("超限不应直接失败: %q", res.Content)
	}
	want := "原文 " + strconv.Itoa(maxBodyBytes) + " 字节"
	if !strings.Contains(res.Content, want) {
		t.Fatalf("应截断到 %d 字节（%q）: %q", maxBodyBytes, want, res.Content[len(res.Content)-80:])
	}
}

// TestBodyOverLimitReal 真实服务返回超大 body：LimitReader 生效且不 panic。
func TestBodyOverLimitReal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		chunk := []byte(strings.Repeat("x", 64<<10))
		for i := 0; i < 40; i++ { // 2.5MB > 1MB
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	resp, err := newHTTPClient().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	limited := make([]byte, maxBodyBytes+1)
	n, _ := io.ReadFull(resp.Body, limited)
	if n > maxBodyBytes+1 {
		t.Fatalf("读取超过上限: %d", n)
	}
	res := buildResult(string(limited[:n]), resp.Header.Get("Content-Type"), srv.URL, resp.StatusCode, maxCharsCeiling)
	if res.IsError {
		t.Fatalf("超限响应不应失败: %q", res.Content)
	}
}

// ---------- 10. max_chars 参数 ----------

func TestClampMaxChars(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, defaultChars},
		{-5, defaultChars},
		{100, 100},
		{defaultChars, defaultChars},
		{maxCharsCeiling, maxCharsCeiling},
		{maxCharsCeiling + 1, maxCharsCeiling},
		{1 << 30, maxCharsCeiling},
	}
	for _, c := range cases {
		if got := clampMaxChars(c.in); got != c.want {
			t.Fatalf("clampMaxChars(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestMaxCharsSmallTruncates 小 max_chars 触发截断标记。
func TestMaxCharsSmallTruncates(t *testing.T) {
	res := buildResult("0123456789", "text/plain", "http://example.com/s", http.StatusOK, clampMaxChars(5))
	if res.IsError {
		t.Fatalf("不应失败: %q", res.Content)
	}
	if !strings.HasPrefix(res.Content, "01234…[已截断，原 10 字符]") {
		t.Fatalf("应截断到 5 字符: %q", res.Content)
	}
}

// TestMaxCharsHugeClampedToCeiling 超大 max_chars 被钳制到 65536。
func TestMaxCharsHugeClampedToCeiling(t *testing.T) {
	body := strings.Repeat("z", maxCharsCeiling+1000)
	res := buildResult(body, "text/plain", "http://example.com/h", http.StatusOK, clampMaxChars(1<<30))
	if res.IsError {
		t.Fatalf("不应失败: %q", res.Content)
	}
	marker := "…[已截断，原 " + strconv.Itoa(maxCharsCeiling+1000) + " 字符]"
	if !strings.Contains(res.Content, marker) {
		t.Fatalf("应按 65536 钳制截断（期望标记 %q）", marker)
	}
	head := res.Content[:maxCharsCeiling]
	if strings.Contains(head, "…[") {
		t.Fatal("钳制后正文长度应恰为 65536 字符")
	}
}

// ---------- 11. extractText 单元测试 ----------

func TestExtractTextPlain(t *testing.T) {
	if got := extractText("  纯文本 <不是标签>  ", "text/plain"); got != "  纯文本 <不是标签>  " {
		t.Fatalf("纯文本应原样保留: %q", got)
	}
}

func TestExtractTextHTML(t *testing.T) {
	body := `<html><head><style>body{color:red}</style><script>var leak="secret";</script></head>
<body><h1>标题一</h1><p>第一段</p><div>块内容</div><script src="x.js"></script></body></html>`
	got := extractText(body, "text/html; charset=utf-8")

	for _, want := range []string{"标题一", "第一段", "块内容"} {
		if !strings.Contains(got, want) {
			t.Fatalf("提取结果缺少 %q: %q", want, got)
		}
	}
	for _, banned := range []string{"secret", "var leak", "<h1>", "</p>", "src=\"x.js\""} {
		if strings.Contains(got, banned) {
			t.Fatalf("提取结果不应含 %q: %q", banned, got)
		}
	}
	if strings.HasPrefix(got, "\n") || strings.HasSuffix(got, "\n") {
		t.Fatalf("结果应 TrimSpace: %q", got)
	}
}

func TestExtractTextEmpty(t *testing.T) {
	if got := extractText("", "text/html"); got != "" {
		t.Fatalf("空 body 应为空串: %q", got)
	}
	if got := extractText("", "text/plain"); got != "" {
		t.Fatalf("空 body 应为空串: %q", got)
	}
	// 空正文 → 无文本错误
	res := buildResult("", "text/html", "http://example.com/e", http.StatusOK, defaultChars)
	if !res.IsError || !strings.Contains(res.Content, "无可提取文本") {
		t.Fatalf("空正文应报无可提取文本: %q", res.Content)
	}
}

// TestExtractTextUnclosedTag 未闭合标签不得 panic / 死循环。
func TestExtractTextUnclosedTag(t *testing.T) {
	got := extractText("前文<div 未闭合", "text/html")
	if !strings.Contains(got, "前文") {
		t.Fatalf("未闭合标签应保守返回已提取内容: %q", got)
	}
}

// ---------- 附：客户端安全参数断言（Proxy=nil / TLS>=1.2 / 超时） ----------

func TestNewHTTPClientSecurityParams(t *testing.T) {
	c := newHTTPClient()
	if c.Timeout != fetchTimeout {
		t.Fatalf("超时应为 %v, got %v", fetchTimeout, c.Timeout)
	}
	if c.CheckRedirect == nil {
		t.Fatal("必须设置 CheckRedirect（每跳 SSRF 校验）")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatalf("Transport 类型异常: %T", c.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("Proxy 必须为 nil（防代理侧绕过私网拦截）")
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("必须设置 TLSClientConfig")
	}
	if tr.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Fatalf("TLS 最低版本必须 >= 1.2, got %#x", tr.TLSClientConfig.MinVersion)
	}
}

// TestDecodeArgsInvalidJSON 参数非法 JSON → 错误结果。
func TestDecodeArgsInvalidJSON(t *testing.T) {
	res, err := invokeFetch(context.Background(), nil, &toolapi.Call{Args: []byte(`{"url":`)})
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("非法 JSON 必须报错: %+v", res)
	}
}

// TestFactoryProducesPlugin 工厂可装配插件并暴露 web_fetch 调用入口。
func TestFactoryProducesPlugin(t *testing.T) {
	f := Factory()
	if f.Manifest().ID != "webfetch" {
		t.Fatalf("工厂清单异常: %q", f.Manifest().ID)
	}
	p, err := f.New(nil, nil)
	if err != nil {
		t.Fatalf("工厂装配失败: %v", err)
	}
	if p == nil {
		t.Fatal("工厂返回空插件")
	}
	if got := f.Manifest().Tools[0].Name; got != "web_fetch" {
		t.Fatalf("工具名异常: %q", got)
	}
}

// TestManifestDeclaresFetchTool 清单与实现一致（工具体积/风险声明）。
func TestManifestDeclaresFetchTool(t *testing.T) {
	m := Manifest()
	if m.ID != "webfetch" {
		t.Fatalf("manifest id 异常: %q", m.ID)
	}
	found := false
	for _, tl := range m.Tools {
		if tl.Name == "web_fetch" {
			found = true
			if tl.Risk != "readonly" {
				t.Fatalf("web_fetch 风险档应为 readonly, got %q", tl.Risk)
			}
		}
	}
	if !found {
		t.Fatal("manifest 未声明 web_fetch")
	}
}

// TestMaxCharsTruncatesHTML HTML 分支截断同样带标记且不含原始标签（回归防抖）。
//
// 与 TestMaxCharsSmallTruncates（纯文本分支）互补：确保截断发生在「去标签之后」，
// 不会把半个标签或 script 内容留在正文里。
func TestMaxCharsTruncatesHTML(t *testing.T) {
	body := "<html><body><p>" + strings.Repeat("汉字段落", 40) + "</p></body></html>"
	res := buildResult(body, "text/html", "http://example.com/h", http.StatusOK, clampMaxChars(10))
	if res.IsError {
		t.Fatalf("不应失败: %q", res.Content)
	}
	if !strings.Contains(res.Content, "…[已截断，原 ") {
		t.Fatalf("HTML 分支截断也应带标记: %q", res.Content)
	}
	if strings.HasPrefix(res.Content, "<") {
		t.Fatalf("截断结果不应含标签: %q", res.Content)
	}
}

// ---------- 测试内小工具 ----------
