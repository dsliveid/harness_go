package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

// 本文件是阶段 C 交互能力增量：click / type / scroll / screenshot。
// 全部复用 browser.go 的 withPage（起浏览器 → 抓 ws URL → 开 target → dialCDP → navigate），
// 与 browser_text 共享同一套降级口径与子进程回收保证。

// maxBase64Len 无 path 落盘时回传 base64 的上限（超出截断并提示用 path 落盘）。
const maxBase64Len = 200_000

// pageURLOrBlank 可选 url 的兜底（未给 url 时只开空白页执行脚本）。
func pageURLOrBlank(raw string) (string, bool) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "about:blank", true
	}
	if !isHTTPURL(u) {
		return "", false
	}
	return u, true
}

// isHTTPURL 仅放行 http/https（与 browser_text 同口径）。
func isHTTPURL(raw string) bool {
	lower := strings.ToLower(raw)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// browserCtx 浏览器操作统一超时（导航 + 等渲染 + 脚本 + 截图）。
func browserCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 60*time.Second)
}

// jsString 把 Go 字符串安全嵌入 JS 字面量。
func jsString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// waitMS 解析 wait_ms（默认 def，上限 15s，与 browser_text 一致）。
func waitMS(v, def int) time.Duration {
	if v <= 0 {
		v = def
	}
	if v > 15000 {
		v = 15000
	}
	return time.Duration(v) * time.Millisecond
}

// ---------- browser_click ----------

// invokeClick 点击匹配 CSS 选择器的元素。
func invokeClick(ctx context.Context, h toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		URL      string `json:"url"`
		Selector string `json:"selector"`
		WaitMS   int    `json:"wait_ms"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("browser_click: %v", err), nil
	}
	sel := strings.TrimSpace(args.Selector)
	if sel == "" {
		return pluginkit.ErrorResult("browser_click: selector 不能为空（需 CSS 选择器）"), nil
	}
	pageURL, ok := pageURLOrBlank(args.URL)
	if !ok {
		return pluginkit.ErrorResult("browser_click: URL 非法（仅 http/https）"), nil
	}
	bin := hostOf(h).probe()
	if bin == "" {
		return pluginkit.ErrorResult("browser_click: 无头浏览器不可用（未检测到 Chromium 系可执行文件）"), nil
	}
	ctxTimeout, cancel := browserCtx(ctx)
	defer cancel()

	var out struct {
		Found bool   `json:"found"`
		Tag   string `json:"tag"`
		Final string `json:"final"`
	}
	err := withPage(ctxTimeout, bin, pageURL, waitMS(args.WaitMS, 1500), func(c *cdp) error {
		expr := fmt.Sprintf(`(() => {
			const el = document.querySelector(%s);
			if (!el) return {found:false, tag:""};
			el.click();
			return {found:true, tag:(el.tagName||"").toLowerCase()};
		})()`, jsString(sel))
		raw, err := evalValue(ctxTimeout, c, expr)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return err
		}
		if !out.Found {
			// 明确失败：列出未命中的选择器（绝不静默成功）
			return fmt.Errorf("选择器未命中任何元素: %s", sel)
		}
		// 点击可能触发导航，容错读取最终地址
		time.Sleep(300 * time.Millisecond)
		if rawURL, err := evalValue(ctxTimeout, c, `window.location.href`); err == nil {
			_ = json.Unmarshal(rawURL, &out.Final)
		}
		return nil
	})
	if err != nil {
		return pluginkit.ErrorResult("browser_click: %v", err), nil
	}
	msg := fmt.Sprintf("已点击 %s（元素 <%s>）", sel, out.Tag)
	if out.Final != "" && out.Final != pageURL {
		msg += fmt.Sprintf("；当前地址 %s", out.Final)
	}
	return pluginkit.TextResult("%s", msg), nil
}

// ---------- browser_type ----------

// invokeType 聚焦元素、设置值并派发 input/change 事件。
func invokeType(ctx context.Context, h toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		URL      string `json:"url"`
		Selector string `json:"selector"`
		Text     string `json:"text"`
		WaitMS   int    `json:"wait_ms"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("browser_type: %v", err), nil
	}
	sel := strings.TrimSpace(args.Selector)
	if sel == "" {
		return pluginkit.ErrorResult("browser_type: selector 不能为空（需 CSS 选择器）"), nil
	}
	pageURL, ok := pageURLOrBlank(args.URL)
	if !ok {
		return pluginkit.ErrorResult("browser_type: URL 非法（仅 http/https）"), nil
	}
	bin := hostOf(h).probe()
	if bin == "" {
		return pluginkit.ErrorResult("browser_type: 无头浏览器不可用（未检测到 Chromium 系可执行文件）"), nil
	}
	ctxTimeout, cancel := browserCtx(ctx)
	defer cancel()

	var out struct {
		Found bool   `json:"found"`
		Value string `json:"value"`
	}
	err := withPage(ctxTimeout, bin, pageURL, waitMS(args.WaitMS, 1500), func(c *cdp) error {
		expr := fmt.Sprintf(`(() => {
			const el = document.querySelector(%s);
			if (!el) return {found:false, value:""};
			const text = %s;
			el.focus();
			if ('value' in el) { el.value = text; } else { el.textContent = text; }
			el.dispatchEvent(new Event('input', {bubbles:true}));
			el.dispatchEvent(new Event('change', {bubbles:true}));
			return {found:true, value:String('value' in el ? el.value : el.textContent)};
		})()`, jsString(sel), jsString(args.Text))
		raw, err := evalValue(ctxTimeout, c, expr)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return err
		}
		if !out.Found {
			return fmt.Errorf("选择器未命中任何元素: %s", sel)
		}
		return nil
	})
	if err != nil {
		return pluginkit.ErrorResult("browser_type: %v", err), nil
	}
	return pluginkit.TextResult("已输入到 %s，当前值 %q", sel, pluginkit.TruncateRunes(out.Value, 500)), nil
}

// ---------- browser_scroll ----------

// invokeScroll 滚动页面（up/down/top/bottom），返回滚动后的 scrollY。
func invokeScroll(ctx context.Context, h toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		URL       string `json:"url"`
		Direction string `json:"direction"`
		Pixels    int    `json:"pixels"`
		WaitMS    int    `json:"wait_ms"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("browser_scroll: %v", err), nil
	}
	dir := strings.ToLower(strings.TrimSpace(args.Direction))
	if dir == "" {
		dir = "down"
	}
	switch dir {
	case "up", "down", "top", "bottom":
	default:
		return pluginkit.ErrorResult("browser_scroll: direction 非法（可选 up/down/top/bottom，实际 %q）", args.Direction), nil
	}
	pageURL, ok := pageURLOrBlank(args.URL)
	if !ok {
		return pluginkit.ErrorResult("browser_scroll: URL 非法（仅 http/https）"), nil
	}
	bin := hostOf(h).probe()
	if bin == "" {
		return pluginkit.ErrorResult("browser_scroll: 无头浏览器不可用（未检测到 Chromium 系可执行文件）"), nil
	}
	ctxTimeout, cancel := browserCtx(ctx)
	defer cancel()

	// pixels 未给（<=0）→ 默认一屏（window.innerHeight）
	px := "window.innerHeight"
	if args.Pixels > 0 {
		px = fmt.Sprintf("%d", args.Pixels)
	}
	var out struct {
		ScrollY      float64 `json:"scrollY"`
		Viewport     float64 `json:"viewport"`
		ScrollHeight float64 `json:"scrollHeight"`
	}
	err := withPage(ctxTimeout, bin, pageURL, waitMS(args.WaitMS, 1200), func(c *cdp) error {
		expr := fmt.Sprintf(`(() => {
			const px = %s;
			const dir = %s;
			if (dir === 'top') { window.scrollTo(0, 0); }
			else if (dir === 'bottom') { window.scrollTo(0, document.documentElement.scrollHeight); }
			else { window.scrollBy(0, dir === 'up' ? -px : px); }
			return {
				scrollY: window.scrollY,
				viewport: window.innerHeight,
				scrollHeight: document.documentElement.scrollHeight
			};
		})()`, px, jsString(dir))
		raw, err := evalValue(ctxTimeout, c, expr)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &out)
	})
	if err != nil {
		return pluginkit.ErrorResult("browser_scroll: %v", err), nil
	}
	return pluginkit.TextResult("已向 %s 滚动：scrollY=%.0f（视口 %.0f / 文档高 %.0f）",
		dir, out.ScrollY, out.Viewport, out.ScrollHeight), nil
}

// ---------- browser_screenshot ----------

// invokeScreenshot 截取页面 PNG（可选落盘到工作区）。
func invokeScreenshot(ctx context.Context, h toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		URL      string `json:"url"`
		Path     string `json:"path"`
		FullPage bool   `json:"full_page"`
		WaitMS   int    `json:"wait_ms"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("browser_screenshot: %v", err), nil
	}
	pageURL, ok := pageURLOrBlank(args.URL)
	if !ok {
		return pluginkit.ErrorResult("browser_screenshot: URL 非法（仅 http/https）"), nil
	}
	rel := strings.TrimSpace(args.Path)
	rel = strings.ReplaceAll(rel, "\\", "/")
	if rel != "" {
		// 越权检查：绝对路径 / 盘符 / 根前缀 / 上跳逃逸一律拒绝（工作区相对路径）
		if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "~") ||
			strings.HasPrefix(filepath.Clean(rel), "..") || filepath.VolumeName(rel) != "" {
			return pluginkit.ErrorResult("browser_screenshot: 落盘路径越界（必须是工作区相对路径）: %s", args.Path), nil
		}
	}
	bin := hostOf(h).probe()
	if bin == "" {
		return pluginkit.ErrorResult("browser_screenshot: 无头浏览器不可用（未检测到 Chromium 系可执行文件）"), nil
	}
	ctxTimeout, cancel := browserCtx(ctx)
	defer cancel()

	var b64 string
	err := withPage(ctxTimeout, bin, pageURL, waitMS(args.WaitMS, 1500), func(c *cdp) error {
		params := map[string]any{"format": "png"}
		if args.FullPage {
			// 整页：先取内容尺寸做 clip，再 captureBeyondViewport
			raw, err := c.call(ctxTimeout, "Page.getLayoutMetrics", nil)
			if err != nil {
				return fmt.Errorf("取页面布局失败: %w", err)
			}
			var m struct {
				CSSContentSize struct {
					Width  float64 `json:"width"`
					Height float64 `json:"height"`
				} `json:"cssContentSize"`
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				return err
			}
			if m.CSSContentSize.Width > 0 && m.CSSContentSize.Height > 0 {
				params["clip"] = map[string]any{
					"x": 0, "y": 0, "width": m.CSSContentSize.Width, "height": m.CSSContentSize.Height, "scale": 1,
				}
				params["captureBeyondViewport"] = true
			}
		}
		raw, err := c.call(ctxTimeout, "Page.captureScreenshot", params)
		if err != nil {
			return err
		}
		var out struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return err
		}
		if out.Data == "" {
			return fmt.Errorf("截图数据为空")
		}
		b64 = out.Data
		return nil
	})
	if err != nil {
		return pluginkit.ErrorResult("browser_screenshot: %v", err), nil
	}

	png, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return pluginkit.ErrorResult("browser_screenshot: base64 解码失败: %v", err), nil
	}
	mode := "视口"
	if args.FullPage {
		mode = "整页"
	}
	if rel != "" {
		ws := hostOf(h).Workspace()
		if ws == nil {
			return pluginkit.ErrorResult("browser_screenshot: 工作区不可用，无法落盘"), nil
		}
		if _, err := ws.Resolve(rel); err != nil {
			return pluginkit.ErrorResult("browser_screenshot: 落盘路径越界（%v）: %s", err, args.Path), nil
		}
		if err := ws.WriteFile(ctxTimeout, rel, png, 0o644); err != nil {
			return pluginkit.ErrorResult("browser_screenshot: 写文件失败: %v", err), nil
		}
		return pluginkit.TextResult("已保存%s截图（PNG，%d 字节）到 %s\n[来源 %s]", mode, len(png), rel, pageURL), nil
	}
	if len(b64) > maxBase64Len {
		return pluginkit.TextResult("%s截图（PNG，%d 字节）base64 过长已省略；请传 path 落盘。\nbase64 前缀: %s",
			mode, len(png), b64[:maxBase64Len]), nil
	}
	return pluginkit.TextResult("%s截图（PNG，%d 字节）base64:\n%s\n[来源 %s]", mode, len(png), b64, pageURL), nil
}
