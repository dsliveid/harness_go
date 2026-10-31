package mcp

// HTTP 插件协议（阶段 E / 插件体系设计 §2「HTTP 插件」）。
//
// 与 MCP 同为「外部插件的一种传输实现」：远端服务暴露
//
//	GET  {base}/manifest.json   → toolapi.Manifest（kind=http，tools 声明工具集）
//	POST {base}/tools/call      → {"name","arguments"} → {"content":[{type:text,...}],"is_error"}
//
// content 复用 MCP 内容形状，使 HTTP 插件与 MCP/内置工具共用同一包装与审批链路。
// Manifest 是唯一一套格式（插件体系设计 §3.2）：远端发现的 manifest 与内置插件的
// //go:embed、市场包内的 manifest.json 完全同构。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"harness-go/pkg/toolapi"
)

// HTTPManifestEndpoint / HTTPToolCallEndpoint HTTP 插件协议的固定路径。
const (
	HTTPManifestEndpoint = "/manifest.json"
	HTTPToolCallEndpoint = "/tools/call"
)

// HTTPClient HTTP 插件客户端。
type HTTPClient struct {
	// BaseURL 服务基址（不含 manifest/call 路径）。
	BaseURL string
	// Headers 附加请求头（鉴权等）。
	Headers map[string]string
	// HTTPClient 可注入（测试）；nil 用默认。
	HTTPClient *http.Client
}

func (c *HTTPClient) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// do 执行带附加头的 JSON 请求。
func (c *HTTPClient) do(ctx context.Context, method, url string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("mcp/http: 序列化请求失败: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return fmt.Errorf("mcp/http: 构造请求失败: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("mcp/http: 请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("mcp/http: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("mcp/http: 响应解析失败: %w", err)
	}
	return nil
}

// FetchManifest 发现远端插件清单（manifest 发现协议）。
func (c *HTTPClient) FetchManifest(ctx context.Context) (*toolapi.Manifest, error) {
	var m toolapi.Manifest
	base := strings.TrimRight(c.BaseURL, "/")
	if err := c.do(ctx, http.MethodGet, base+HTTPManifestEndpoint, nil, &m); err != nil {
		return nil, err
	}
	if m.Kind == "" {
		m.Kind = toolapi.KindHTTP
	}
	m.Kind = toolapi.KindHTTP // 发现即定型：HTTP 插件不允许远端自称 builtin
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("mcp/http: 远端 manifest 非法: %w", err)
	}
	return &m, nil
}

// httpCallResult 工具调用响应（content 复用 MCP 文本形状）。
type httpCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"is_error"`
}

// CallTool 调用远端工具。
func (c *HTTPClient) CallTool(ctx context.Context, rawName string, args []byte) (string, bool, error) {
	var argsMap map[string]any
	if len(args) > 0 && json.Valid(args) {
		_ = json.Unmarshal(args, &argsMap)
	}
	var res httpCallResult
	base := strings.TrimRight(c.BaseURL, "/")
	if err := c.do(ctx, http.MethodPost, base+HTTPToolCallEndpoint,
		map[string]any{"name": rawName, "arguments": argsMap}, &res); err != nil {
		return "", false, err
	}
	var b strings.Builder
	for _, c := range res.Content {
		if c.Type == "text" || c.Type == "" {
			b.WriteString(c.Text)
		} else {
			b.WriteString("[非文本内容（" + c.Type + "），已省略]\n")
		}
	}
	return b.String(), res.IsError, nil
}

// HTTPPlugin 远端 HTTP 插件的 toolapi.Plugin 适配：清单在注册期已发现，
// 每次调用经 HTTPClient 转发。
type HTTPPlugin struct {
	m       *toolapi.Manifest
	client  *HTTPClient
	tools   []toolapi.Tool
	prefix  string
	id      string
	timeout time.Duration
	onErr   func(err error) // 观测钩子（可空）
}

// NewHTTPPlugin 发现远端 manifest 并构建统一工具（namespace http:<id>）。
//
// description 加 [server] 前缀与截断、schema 兜底、风险取 manifest 声明
// （工具级 Risk 缺省回落插件 DefaultRisk，再回落 write）。
func NewHTTPPlugin(ctx context.Context, id, name, baseURL string, headers map[string]string) (*HTTPPlugin, error) {
	client := &HTTPClient{BaseURL: baseURL, Headers: headers}
	m, err := client.FetchManifest(ctx)
	if err != nil {
		return nil, newError(ErrCodeConnectFailed, id, "manifest 发现失败: %v", err)
	}
	if len(m.Tools) == 0 {
		return nil, newError(ErrCodeProtocolError, id, "远端 manifest 未声明任何工具")
	}
	p := &HTTPPlugin{m: m, client: client, prefix: "http", id: id, timeout: 30 * time.Second}
	for _, s := range m.Tools {
		risk := string(s.Risk)
		if risk == "" {
			risk = "write"
		}
		// 工具名段用 ID（稳定 slug，与本地工具包同口径），展示名进描述前缀。
		spec := buildSpec("http", "http:"+id, id, s.Name, s.Description, string(s.SchemaJSON), risk)
		p.tools = append(p.tools, &httpTool{p: p, rawName: s.Name, spec: spec})
	}
	return p, nil
}

// Manifest 实现 toolapi.Plugin。
func (p *HTTPPlugin) Manifest() toolapi.Manifest { return *p.m }

// Tools 实现 toolapi.Plugin。
func (p *HTTPPlugin) Tools() []toolapi.Tool { return p.tools }

// Health 实现 toolapi.Plugin（HTTP 插件按需探测；注册期视为可用）。
func (p *HTTPPlugin) Health() toolapi.Health {
	return toolapi.Health{Status: toolapi.HealthAvailable, CheckedAt: time.Now()}
}

// Start 实现 toolapi.Plugin（无长连接，幂等空操作）。
func (p *HTTPPlugin) Start(context.Context) error { return nil }

// Stop 实现 toolapi.Plugin（无长连接，幂等空操作）。
func (p *HTTPPlugin) Stop(context.Context) error { return nil }

// httpTool 单个 HTTP 插件工具（与内置/MCP 工具同构）。
type httpTool struct {
	p       *HTTPPlugin
	rawName string
	spec    toolapi.Spec
}

// Spec 实现统一契约。
func (t *httpTool) Spec() toolapi.Spec { return t.spec }

// Invoke 实现统一契约。
func (t *httpTool) Invoke(ctx context.Context, call *toolapi.Call) (*toolapi.Result, error) {
	cctx, cancel := context.WithTimeout(ctx, t.p.timeout)
	defer cancel()
	text, isErr, err := t.p.client.CallTool(cctx, t.rawName, call.Args)
	if err != nil {
		if t.p.onErr != nil {
			t.p.onErr(err)
		}
		return &toolapi.Result{Content: err.Error(), IsError: true,
			Meta: map[string]any{"http_plugin": t.p.id}}, nil
	}
	return &toolapi.Result{Content: text, IsError: isErr}, nil
}

// 编译期保证：HTTPPlugin 满足插件契约。
var _ toolapi.Plugin = (*HTTPPlugin)(nil)
