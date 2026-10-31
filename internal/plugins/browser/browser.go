package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

// 可执行文件候选（探测顺序）。
var candidates = []string{"chrome", "chromium", "msedge", "google-chrome", "chromium-browser"}

// plugin 浏览器插件（探测结果缓存 + 线程安全）。
type plugin struct {
	toolapi.Host
	mu     sync.Mutex
	bin    string // 探测到的可执行文件（空 = 未找到）
	probed bool
}

// Manifest 实现 toolapi.Plugin。
func (p *plugin) Manifest() toolapi.Manifest { return Manifest() }

// Tools 实现 toolapi.Plugin。
func (p *plugin) Tools() []toolapi.Tool {
	return []toolapi.Tool{
		&kitToolAdapter{p: p, name: "browser_probe", fn: invokeProbe},
		&kitToolAdapter{p: p, name: "browser_text", fn: invokeText},
	}
}

// Start 实现 toolapi.Plugin（启动即探测一次，健康态据此判定）。
func (p *plugin) Start(context.Context) error { p.probe(); return nil }

// Stop 实现 toolapi.Plugin（幂等空操作）。
func (p *plugin) Stop(context.Context) error { return nil }

// Health 实现 toolapi.Plugin：Chromium 缺失 → unavailable（显式降级，UI 显示缺什么）。
func (p *plugin) Health() toolapi.Health {
	bin := p.probe()
	if bin == "" {
		return toolapi.Health{
			Status:    toolapi.HealthUnavailable,
			Message:   "未检测到 Chromium 系浏览器（chrome/chromium/msedge）；请安装或配置路径",
			CheckedAt: time.Now(),
		}
	}
	return toolapi.Health{Status: toolapi.HealthAvailable, Message: bin, CheckedAt: time.Now()}
}

// probe 探测（幂等缓存）。
func (p *plugin) probe() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.probed {
		return p.bin
	}
	p.probed = true
	// ① 配置覆盖
	if cfg := p.Host.Config(); cfg != nil {
		if v := cfg.String("browser.executable", ""); v != "" {
			if _, err := os.Stat(v); err == nil {
				p.bin = v
				return v
			}
		}
	}
	// ② PATH 查找
	for _, name := range candidates {
		if path, err := exec.LookPath(name); err == nil {
			p.bin = path
			return path
		}
	}
	// ③ 常见安装目录（Windows Edge 几乎必有）
	for _, path := range commonPaths {
		if _, err := os.Stat(path); err == nil {
			p.bin = path
			return path
		}
	}
	return ""
}

var commonPaths = []string{
	`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
	`C:\Program Files\Google\Chrome\Application\chrome.exe`,
	`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
	"/usr/bin/chromium", "/usr/bin/chromium-browser", "/usr/bin/google-chrome",
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
}

// invokeProbe 探测工具。
func invokeProbe(ctx context.Context, h toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	p := hostOf(h)
	bin := p.probe()
	if bin == "" {
		return pluginkit.ErrorResult("无头浏览器不可用：未检测到 chrome/chromium/msedge（安装后重试，或设置配置 browser.executable）"), nil
	}
	return pluginkit.TextResult("无头浏览器可用：%s", bin), nil
}

// invokeText 无头渲染并提取可见文本。
func invokeText(ctx context.Context, h toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		URL    string `json:"url"`
		WaitMS int    `json:"wait_ms"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("browser_text: %v", err), nil
	}
	u, err := url.Parse(strings.TrimSpace(args.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return pluginkit.ErrorResult("browser_text: URL 非法（仅 http/https）"), nil
	}
	bin := hostOf(h).probe()
	if bin == "" {
		return pluginkit.ErrorResult("browser_text: 无头浏览器不可用（未检测到 Chromium 系可执行文件）"), nil
	}
	wait := args.WaitMS
	if wait <= 0 {
		wait = 1500
	}
	if wait > 15000 {
		wait = 15000
	}
	ctxTimeout, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	text, err := renderText(ctxTimeout, bin, args.URL, time.Duration(wait)*time.Millisecond)
	if err != nil {
		return pluginkit.ErrorResult("browser_text: %v", err), nil
	}
	if strings.TrimSpace(text) == "" {
		return pluginkit.ErrorResult("browser_text: 页面渲染后无可提取文本"), nil
	}
	return pluginkit.TextResult("%s\n\n[来源 %s]", pluginkit.TruncateRunes(text, 20000), args.URL), nil
}

// renderText 起 headless 进程 → CDP WS → navigate → evaluate。
func renderText(ctx context.Context, bin, pageURL string, wait time.Duration) (string, error) {
	// --remote-debugging-port=0：内核选端口并把 ws URL 打到 stderr
	cmd := exec.Command(bin, "--headless=new", "--disable-gpu", "--no-first-run",
		"--remote-debugging-port=0", "about:blank")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("启动浏览器失败: %w", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// ① 从 stderr 读取浏览器级调试端点（含端口）
	browserWS := ""
	deadline := time.Now().Add(10 * time.Second)
	sc := bufio.NewScanner(stderr)
	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		if i := strings.Index(line, "ws://"); i >= 0 {
			browserWS = strings.TrimSpace(line[i:])
			break
		}
	}
	if browserWS == "" {
		return "", fmt.Errorf("读取调试端口失败（浏览器未输出 ws URL）")
	}
	// ② 创建页面 target：浏览器级端点不支持 Page 域，必须先经 /json/new 开页面
	port := extractDebugPort(browserWS)
	pageWS, err := createPageTarget(ctx, port, pageURL)
	if err != nil {
		return "", err
	}
	// ③ 连接页面 target 的 WS（Page/Runtime 域可用）
	cdp, err := dialCDP(ctx, pageWS)
	if err != nil {
		return "", err
	}
	defer func() { _ = cdp.Close() }()
	if _, err := cdp.call(ctx, "Page.enable", nil); err != nil {
		return "", err
	}
	// 显式导航（/json/new 已带 url，重复导航保证确定性）+ 等渲染
	if _, err := cdp.call(ctx, "Page.navigate", map[string]any{"url": pageURL}); err != nil {
		return "", fmt.Errorf("导航失败: %w", err)
	}
	time.Sleep(wait)
	res, err := cdp.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    "document.body ? document.body.innerText : ''",
		"returnByValue": true,
		"awaitPromise":  false,
	})
	if err != nil {
		return "", fmt.Errorf("取文本失败: %w", err)
	}
	var out struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	return out.Result.Value, nil
}

func hostOf(h toolapi.Host) *plugin {
	if p, ok := h.(*plugin); ok {
		return p
	}
	// 测试/非标准装配：退化为独立探测实例
	return &plugin{Host: h}
}

// kitToolAdapter 把函数适配为 toolapi.Tool（browser 的工具数少，不复用 PluginFromSpecs）。
type kitToolAdapter struct {
	p    *plugin
	name string
	fn   func(ctx context.Context, h toolapi.Host, call *toolapi.Call) (*toolapi.Result, error)
}

func (t *kitToolAdapter) Spec() toolapi.Spec {
	spec, err := pluginkit.SpecFromManifest(Manifest(), t.name)
	if err != nil {
		panic(err)
	}
	return spec
}

func (t *kitToolAdapter) Invoke(ctx context.Context, call *toolapi.Call) (*toolapi.Result, error) {
	return t.fn(ctx, t.p, call)
}

// extractDebugPort 从浏览器 ws URL 提取调试端口（ws://127.0.0.1:<port>/devtools/...）。
func extractDebugPort(wsURL string) string {
	rest := strings.TrimPrefix(wsURL, "ws://")
	i := strings.Index(rest, "/")
	host := rest
	if i >= 0 {
		host = rest[:i]
	}
	if j := strings.LastIndex(host, ":"); j >= 0 {
		return host[j+1:]
	}
	return "9222"
}

// createPageTarget 经 HTTP /json/new 创建页面 target，返回其 WS URL。
func createPageTarget(ctx context.Context, port, pageURL string) (string, error) {
	req, err := newRequest(ctx, "http://127.0.0.1:"+port+"/json/new?"+url.QueryEscape(pageURL))
	if err != nil {
		return "", err
	}
	req.Method = http.MethodPut // 新版 Chromium 的 /json/new 仅接受 PUT（GET 返回纯文本错误）
	resp, err := client().Do(req)
	if err != nil {
		return "", fmt.Errorf("创建页面 target 失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("解析 target 失败: %w", err)
	}
	if out.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("target 未返回 webSocketDebuggerURL")
	}
	return out.WebSocketDebuggerURL, nil
}

func newRequest(ctx context.Context, target string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
}

func client() *http.Client { return &http.Client{Timeout: 10 * time.Second} }

var _ = io.Discard
