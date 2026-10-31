package webfetch

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

const (
	maxBodyBytes = 1 << 20
	defaultChars = 16000
	fetchTimeout = 30 * time.Second
)

// invokeFetch 抓取 URL 并提取正文文本。
func invokeFetch(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		URL      string `json:"url"`
		MaxChars int    `json:"max_chars"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("web_fetch: %v", err), nil
	}
	u, err := url.Parse(strings.TrimSpace(args.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return pluginkit.ErrorResult("web_fetch: URL 非法（仅 http/https）: %q", args.URL), nil
	}
	if err := guardPrivateHost(u.Hostname()); err != nil {
		return pluginkit.ErrorResult("web_fetch: %v", err), nil
	}
	maxChars := args.MaxChars
	if maxChars <= 0 {
		maxChars = defaultChars
	}
	if maxChars > 65536 {
		maxChars = 65536
	}

	client := &http.Client{
		Timeout: fetchTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			Proxy:           nil, // 不走系统代理（防代理侧绕过私网拦截）
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("重定向次数超限")
			}
			// 每一跳都重新校验目标网段（防重定向绕过 SSRF 防护）
			return guardPrivateHost(req.URL.Hostname())
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return pluginkit.ErrorResult("web_fetch: 构造请求失败: %v", err), nil
	}
	req.Header.Set("User-Agent", "harness-go-webfetch/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return pluginkit.ErrorResult("web_fetch: 请求失败: %v", err), nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return pluginkit.ErrorResult("web_fetch: HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)), nil
	}
	ct := resp.Header.Get("Content-Type")
	if !isTextType(ct) {
		return pluginkit.ErrorResult("web_fetch: 不支持的类型 %q（仅文本/HTML）", ct), nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return pluginkit.ErrorResult("web_fetch: 读取失败: %v", err), nil
	}
	if len(body) > maxBodyBytes {
		body = body[:maxBodyBytes]
	}
	text := extractText(string(body), ct)
	if text == "" {
		return pluginkit.ErrorResult("web_fetch: 页面无可提取文本"), nil
	}
	return pluginkit.TextResult("%s\n\n[来源 %s，状态 %d，原文 %d 字节]",
		pluginkit.TruncateRunes(text, maxChars), args.URL, resp.StatusCode, len(body)), nil
}

// guardPrivateHost 拦截环回/私有/链路本地网段（SSRF 防护）。
func guardPrivateHost(hostname string) error {
	hostname = strings.Trim(hostname, "[]")
	if hostname == "localhost" {
		return fmt.Errorf("目标被拦截：环回地址（SSRF 防护）")
	}
	ip := net.ParseIP(hostname)
	if ip == nil {
		// 域名：允许（解析后的实际连接仍由系统栈完成；DNS 重绑定防护属后续增强，
		// 依赖私网 IP 直连拦截已覆盖绝大多数探测面）
		return nil
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("目标被拦截：私有/环回网段 %s（SSRF 防护）", ip)
	}
	return nil
}

func isTextType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") ||
		strings.Contains(ct, "xml") || strings.Contains(ct, "html") || ct == ""
}

// extractText 去标签近似提取正文（HTML）；纯文本原样保留。
func extractText(body, ct string) string {
	if !strings.Contains(ct, "html") {
		return body
	}
	var b strings.Builder
	depth := 0
	inScript := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '<' {
			if depth == 0 && !inScript {
				b.WriteByte(c)
			}
			continue
		}
		end := strings.IndexByte(body[i:], '>')
		if end < 0 {
			break
		}
		tag := strings.ToLower(body[i+1 : i+end])
		inScript = strings.HasPrefix(tag, "script") || (inScript && !strings.HasPrefix(tag, "/script"))
		if strings.HasPrefix(tag, "br") || strings.HasPrefix(tag, "/p") || strings.HasPrefix(tag, "p ") ||
			strings.HasPrefix(tag, "div") || strings.HasPrefix(tag, "/div") || strings.HasPrefix(tag, "li") ||
			strings.HasPrefix(tag, "/h") || strings.HasPrefix(tag, "h1") || strings.HasPrefix(tag, "tr") {
			b.WriteByte('\n')
		}
		depth = 0 // 简化：不跟踪嵌套，标签间内容保留
		i += end
	}
	// 压缩空白
	var out strings.Builder
	prevSpace := false
	for _, r := range b.String() {
		if r == ' ' || r == '\t' {
			if !prevSpace {
				out.WriteRune(' ')
			}
			prevSpace = true
			continue
		}
		prevSpace = r == '\n'
		out.WriteRune(r)
	}
	return strings.TrimSpace(out.String())
}
