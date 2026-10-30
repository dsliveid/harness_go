package toolapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Kind 插件形态（内置 / MCP / HTTP / 本地工具包 / WASM 预留）。
type Kind string

const (
	// KindBuiltin 编译期注册的内置插件。
	KindBuiltin Kind = "builtin"
	// KindMCP MCP Server（stdio / SSE / HTTP 三传输）。
	KindMCP Kind = "mcp"
	// KindHTTP HTTP 插件（manifest 发现）。
	KindHTTP Kind = "http"
	// KindPackage 本地工具包（子进程 + stdio JSON-RPC，复用 MCP 协议）。
	KindPackage Kind = "package"
	// KindWasm WASM 沙箱（一期不做，仅枚举占位）。
	KindWasm Kind = "wasm"
)

// ProtocolVersion 当前插件协议版本（不兼容变更必须递增）。
const ProtocolVersion = 1

// Manifest 插件清单（**唯一一套**：内置插件用 //go:embed，市场包/ MCP 转换复用同结构）。
type Manifest struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Version         string         `json:"version"`
	Kind            Kind           `json:"kind"`
	Description     string         `json:"description,omitempty"`
	DefaultTier     Tier           `json:"default_tier"`
	ProtocolVersion int            `json:"protocol_version"`
	Author          string         `json:"author,omitempty"`
	Homepage        string         `json:"homepage,omitempty"`
	License         string         `json:"license,omitempty"`
	Signature       string         `json:"signature,omitempty"` // 市场包签名（内置插件为空）
	Requires        []string       `json:"requires,omitempty"`
	Tools           []Spec         `json:"tools,omitempty"`
	Meta            map[string]any `json:"meta,omitempty"`
}

// Validate 校验清单（注册期调用）。
func (m Manifest) Validate() error {
	if strings.TrimSpace(m.ID) == "" {
		return fmt.Errorf("插件 ID 为空")
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("插件 %s 的 Name 为空", m.ID)
	}
	if m.ProtocolVersion <= 0 {
		return fmt.Errorf("插件 %s 的 protocol_version 非法: %d", m.ID, m.ProtocolVersion)
	}
	switch m.Kind {
	case KindBuiltin, KindMCP, KindHTTP, KindPackage, KindWasm:
	default:
		return fmt.Errorf("插件 %s 的 kind 非法: %q", m.ID, m.Kind)
	}
	for _, s := range m.Tools {
		if err := s.Normalized().Validate(); err != nil {
			return fmt.Errorf("插件 %s: %w", m.ID, err)
		}
	}
	return nil
}

// Plugin 插件运行时实例。
type Plugin interface {
	// Start 启动（校验依赖、建立连接等）。
	Start(ctx context.Context) error
	// Stop 停止（释放资源，必须幂等）。
	Stop(ctx context.Context) error
	// Tools 暴露的工具列表（名称与 Manifest 声明必须一致）。
	Tools() []Tool
	// Health 健康检查。
	Health() Health
}

// Factory 插件工厂（cmd/harness-go/plugins.go 是唯一装配点）。
type Factory interface {
	Manifest() Manifest
	New(host Host, cfg Config) (Plugin, error)
}

// FactoryFunc 便于用函数快速实现 Factory。
type FactoryFunc struct {
	M    Manifest
	NewF func(host Host, cfg Config) (Plugin, error)
}

// Manifest 实现 Factory。
func (f FactoryFunc) Manifest() Manifest { return f.M }

// New 实现 Factory。
func (f FactoryFunc) New(host Host, cfg Config) (Plugin, error) {
	if f.NewF == nil {
		return nil, ErrNotImplemented
	}
	return f.NewF(host, cfg)
}

// MarshalManifest 序列化清单（调试与市场包导出）。
func MarshalManifest(m Manifest) (json.RawMessage, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}
