package browser

import (
	"context"
	"strconv"
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

// noBrowserPlugin 构造「探测已完成且未找到浏览器」的插件（强制降级分支）。
func noBrowserPlugin(t *testing.T) *plugin {
	t.Helper()
	p := &plugin{Host: plugintest.New(t)}
	p.mu.Lock()
	p.probed = true
	p.bin = ""
	p.mu.Unlock()
	return p
}

// TestNewToolsWithoutChromium 新增工具与 browser_text 完全同口径：缺失即显式不可用。
func TestNewToolsWithoutChromium(t *testing.T) {
	p := noBrowserPlugin(t)
	cases := []struct {
		name string
		fn   func(context.Context, toolapi.Host, *toolapi.Call) (*toolapi.Result, error)
		args string
	}{
		{"browser_click", invokeClick, `{"url":"https://example.com","selector":"a"}`},
		{"browser_type", invokeType, `{"url":"https://example.com","selector":"input","text":"hi"}`},
		{"browser_scroll", invokeScroll, `{"url":"https://example.com","direction":"down"}`},
		{"browser_screenshot", invokeScreenshot, `{"url":"https://example.com"}`},
	}
	for _, c := range cases {
		res, err := c.fn(t.Context(), p, &toolapi.Call{Args: []byte(c.args)})
		if err != nil {
			t.Fatalf("%s 不应返回 Go error: %v", c.name, err)
		}
		if !res.IsError || !strings.Contains(res.Content, "无头浏览器不可用") {
			t.Fatalf("%s 缺失浏览器时必须显式不可用: %q", c.name, res.Content)
		}
	}
}

// TestNewToolsArgValidation 参数校验发生在探测之前（无浏览器也应先报参数错）。
func TestNewToolsArgValidation(t *testing.T) {
	p := noBrowserPlugin(t)

	// selector 为空
	res, _ := invokeClick(t.Context(), p, &toolapi.Call{Args: []byte(`{"selector":"   "}`)})
	if !res.IsError || !strings.Contains(res.Content, "selector 不能为空") {
		t.Fatalf("空 selector 必须拒绝: %q", res.Content)
	}
	res, _ = invokeType(t.Context(), p, &toolapi.Call{Args: []byte(`{"text":"x"}`)})
	if !res.IsError || !strings.Contains(res.Content, "selector 不能为空") {
		t.Fatalf("browser_type 空 selector 必须拒绝: %q", res.Content)
	}

	// direction 非法
	res, _ = invokeScroll(t.Context(), p, &toolapi.Call{Args: []byte(`{"direction":"left"}`)})
	if !res.IsError || !strings.Contains(res.Content, "direction 非法") {
		t.Fatalf("非法 direction 必须拒绝: %q", res.Content)
	}

	// path 逃逸（上跳 / 绝对路径）——绝不允许写穿工作区
	for _, bad := range []string{`../evil.png`, `../../etc/x.png`, `C:\\Windows\\x.png`, `/etc/x.png`} {
		args := []byte(`{"path":` + strconv.Quote(bad) + `}`)
		res, _ = invokeScreenshot(t.Context(), p, &toolapi.Call{Args: args})
		if !res.IsError || !strings.Contains(res.Content, "越界") {
			t.Fatalf("落盘路径 %q 必须拒绝: %q", bad, res.Content)
		}
	}

	// 非法 URL（非 http/https）
	res, _ = invokeClick(t.Context(), p, &toolapi.Call{Args: []byte(`{"url":"ftp://x","selector":"a"}`)})
	if !res.IsError || !strings.Contains(res.Content, "URL 非法") {
		t.Fatalf("非法 URL 必须拒绝: %q", res.Content)
	}
}

// TestManifestMatchesRegisteredTools manifest 声明与 Tools() 注册必须一一对应（单点真源）。
func TestManifestMatchesRegisteredTools(t *testing.T) {
	p := &plugin{Host: plugintest.New(t)}
	tools := p.Tools()
	declared := map[string]toolapi.Spec{}
	for _, s := range Manifest().Tools {
		declared[s.Name] = s
	}
	if len(tools) != len(declared) {
		t.Fatalf("注册工具数 %d 与 manifest 声明数 %d 不一致", len(tools), len(declared))
	}
	want := []string{"browser_probe", "browser_text", "browser_click", "browser_type", "browser_scroll", "browser_screenshot"}
	for _, name := range want {
		if _, ok := declared[name]; !ok {
			t.Fatalf("manifest 缺少工具声明: %s", name)
		}
	}
	seen := map[string]bool{}
	for _, tl := range tools {
		sp := tl.Spec()
		seen[sp.Name] = true
		if sp.Tier != toolapi.TierOnDemand {
			t.Fatalf("%s 应为 on_demand tier（实际 %q）", sp.Name, sp.Tier)
		}
		switch sp.Name {
		case "browser_click", "browser_type", "browser_screenshot":
			if sp.Risk != toolapi.RiskWrite {
				t.Fatalf("%s 风险级应为 write（实际 %q）", sp.Name, sp.Risk)
			}
		case "browser_scroll", "browser_text", "browser_probe":
			if sp.Risk != toolapi.RiskReadonly {
				t.Fatalf("%s 风险级应为 readonly（实际 %q）", sp.Name, sp.Risk)
			}
		}
	}
	for _, name := range want {
		if !seen[name] {
			t.Fatalf("Tools() 缺少注册: %s", name)
		}
	}
}
