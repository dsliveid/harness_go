package toolapi

import (
	"context"
	"encoding/json"
)

// Call 一次工具调用请求（由内核填好运行上下文后传给工具）。
type Call struct {
	CallID      string          `json:"call_id"`       // 模型给出的 tool_call_id
	RunID       string          `json:"run_id"`        //
	SessionID   string          `json:"session_id"`    //
	ToolEventID string          `json:"tool_event_id"` // 会话库 tool_events 主键，插件可用于回写进度
	Name        string          `json:"name"`
	Args        json.RawMessage `json:"args"`
	AccessMode  string          `json:"access_mode"` // readonly | confirm | full
}

// Result 工具执行结果。
//
// IsError 为 true 时 Content 必须是「给模型看的失败原因」，
// 引擎会把它作为 tool 消息回灌模型（让模型换方案），而不是中断 Run。
type Result struct {
	Content    string         `json:"content"`
	IsError    bool           `json:"is_error,omitempty"`
	BlobRef    string         `json:"blob_ref,omitempty"` // 大结果转 blob 的 hash（空 = 内联）
	CostTokens int            `json:"cost_tokens,omitempty"`
	Meta       map[string]any `json:"meta,omitempty"`
}

// Tool 一个可调用工具。
//
// 实现约束：Invoke 必须是并发安全的；不得直接访问数据库、不得自建 HTTP 服务，
// 一切受控能力经 Host 获取（见 host.go）。
type Tool interface {
	Spec() Spec
	Invoke(ctx context.Context, call *Call) (*Result, error)
}

// ToolFunc 适配函数式工具实现。
type ToolFunc struct {
	S       Spec
	InvokeF func(ctx context.Context, call *Call) (*Result, error)
}

// Spec 实现 Tool。
func (t ToolFunc) Spec() Spec { return t.S }

// Invoke 实现 Tool。
func (t ToolFunc) Invoke(ctx context.Context, call *Call) (*Result, error) {
	if t.InvokeF == nil {
		return nil, ErrNotImplemented
	}
	return t.InvokeF(ctx, call)
}
