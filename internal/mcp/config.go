// Package mcp 实现 MCP 客户端与外部插件接入（阶段 E / M2-MCP）。
//
// 定位（docs/MCP接入设计.md）：MCP 是「外部插件」的一种传输实现，不是独立体系——
// MCP 工具最终被包装为统一 toolapi.Tool 注册进同一注册表，走同一套审批与注入规则。
//
// 三传输（官方 SDK github.com/modelcontextprotocol/go-sdk）：
//   - stdio：内核启动子进程，stdin/stdout 换行分隔 JSON-RPC（本地工具包复用同路径）
//   - sse：HTTP+SSE（旧版协议 2024-11-05）
//   - http：Streamable HTTP（新版，单端点 POST，支持 Mcp-Session-Id）
//
// 本包零 store 依赖：配置的持久化由 store 层完成，本包只消费内存配置。
package mcp

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"harness-go/pkg/toolapi"
)

// Transport MCP 传输类型。
type Transport string

const (
	// TransportStdio 子进程 stdio（换行分隔 JSON-RPC）。
	TransportStdio Transport = "stdio"
	// TransportSSE HTTP+SSE（旧版 2024-11-05）。
	TransportSSE Transport = "sse"
	// TransportHTTP Streamable HTTP（新版）。
	TransportHTTP Transport = "http"
)

// Valid 判断传输类型是否已知。
func (t Transport) Valid() bool {
	return t == TransportStdio || t == TransportSSE || t == TransportHTTP
}

// 默认参数（对齐 docs/MCP接入设计.md §2.3 / §3.1）。
const (
	// DefaultTimeoutMS 单次工具调用超时。
	DefaultTimeoutMS = 120000
	// DefaultMaxOutput 注入模型的输出字节上限。
	DefaultMaxOutput = 32768
	// DefaultHealthIntervalS 主动健康检查间隔（0=关闭）。
	DefaultHealthIntervalS = 60
	// HandshakeTimeout 握手（initialize）超时。
	HandshakeTimeout = 10 * time.Second
	// ListTimeout tools/list 超时。
	ListTimeout = 15 * time.Second
	// MaxDescriptionRunes 工具描述进上下文的长度上限（成本控制，超长截断）。
	MaxDescriptionRunes = 200
)

// ServerConfig 单个 MCP Server 的运行配置（内存形态，持久化在 store 层）。
type ServerConfig struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Transport `json:"transport"`
	// stdio 专属
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"` // 内存态明文；落库由 store 层加密
	Cwd     string            `json:"cwd,omitempty"`
	// 远程（sse / http）
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// 通用
	Enabled         bool     `json:"enabled"`
	TimeoutMS       int      `json:"timeout_ms"`
	MaxOutput       int      `json:"max_output"`
	DefaultRisk     string   `json:"default_risk"`          // readonly|write|command|danger（默认 write）
	AllowTools      []string `json:"allow_tools,omitempty"` // 白名单（空=全部）
	DenyTools       []string `json:"deny_tools,omitempty"`  // 黑名单（优先于白名单）
	HealthIntervalS int      `json:"health_interval_s"`
	// PluginKind 插件形态路由（内部字段，不入库不出 API）：
	// mcp（默认，前缀 mcp__ / 命名空间 mcp:<id>）| package（本地工具包路径，
	// 前缀 pkg__ / 命名空间 pkg:<id>）——同一套连接代码，只差「包从哪来」。
	PluginKind toolapi.Kind `json:"-"`
}

// Normalize 补齐默认值（不改接收者）。
func (c ServerConfig) Normalize() ServerConfig {
	out := c
	out.Name = strings.TrimSpace(out.Name)
	if out.TimeoutMS <= 0 {
		out.TimeoutMS = DefaultTimeoutMS
	}
	if out.MaxOutput <= 0 {
		out.MaxOutput = DefaultMaxOutput
	}
	if out.DefaultRisk == "" {
		out.DefaultRisk = "write"
	}
	if out.HealthIntervalS < 0 {
		out.HealthIntervalS = DefaultHealthIntervalS
	}
	return out
}

// DeploymentPolicy 部署形态约束（docs/MCP接入设计.md §3.3）。
type DeploymentPolicy struct {
	// Mode local | server | cloud
	Mode string
	// AllowStdio server 形态显式放行 stdio（cloud 恒禁止，本字段无效）。
	AllowStdio bool
}

// Validate 校验配置；deployment 按形态约束判定 stdio 可用性。
func (c ServerConfig) Validate(dep DeploymentPolicy) error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("mcp: name 不能为空")
	}
	if !c.Transport.Valid() {
		return fmt.Errorf("mcp: transport 非法: %q（可选 stdio|sse|http）", string(c.Transport))
	}
	switch c.Transport {
	case TransportStdio:
		if strings.TrimSpace(c.Command) == "" {
			return fmt.Errorf("mcp: stdio 传输的 command 不能为空")
		}
		// 部署形态硬约束：cloud 禁止起子进程；server 需显式 allow_stdio。
		switch dep.Mode {
		case "cloud":
			return fmt.Errorf("mcp: cloud 形态禁止 stdio 传输（不允许服务端起子进程），请改用 sse/http 远程端点")
		case "server":
			if !dep.AllowStdio {
				return fmt.Errorf("mcp: server 形态使用 stdio 需显式开启 mcp.allow_stdio")
			}
		}
	case TransportSSE, TransportHTTP:
		if strings.TrimSpace(c.URL) == "" {
			return fmt.Errorf("mcp: %s 传输的 url 不能为空", string(c.Transport))
		}
		u, err := url.Parse(strings.TrimSpace(c.URL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("mcp: url 非法（需 http/https）: %q", c.URL)
		}
	}
	switch c.DefaultRisk {
	case "", "readonly", "write", "command", "danger":
	default:
		return fmt.Errorf("mcp: default_risk 非法: %q", c.DefaultRisk)
	}
	return nil
}

// ServerKey 把展示名规范化为工具名段（小写、非字母数字转 _、截断 24 字符）。
// 工具全名 = mcp__<serverKey>__<toolName>。
func ServerKey(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	key := b.String()
	key = strings.Trim(key, "_")
	if key == "" {
		key = "server"
	}
	if len(key) > 24 {
		key = key[:24]
	}
	return key
}
