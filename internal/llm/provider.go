// Package llm 定义模型调用端口（Provider / Stream / Chunk）与错误模型。
//
// 设计约束（docs/架构设计文档.md §2.3）：
//   - 端口在使用方（agent）以窄接口使用，本包只提供通用定义与适配器
//   - 适配器不得感知 Run/会话/审批等业务概念，只做「请求 → 流式增量」的翻译
//   - 所有错误都要能被上层判定「可重试 / 不可重试」
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Role 消息角色（与 provider 协议对齐的最小集合）。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message 一条对话消息。
type Message struct {
	Role       Role
	Content    string
	Name       string     // tool 消息对应的工具名（部分协议需要）
	ToolCalls  []ToolCall // assistant 发起的工具调用
	ToolCallID string     // tool 消息对应的调用 ID
}

// ToolCall 模型发起的工具调用（完整形态）。
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// ToolSpec 工具定义（仅传 name/description/schema，与 pkg/toolapi.Spec 解耦）。
type ToolSpec struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ChatRequest 一次模型调用请求。
type ChatRequest struct {
	Model       string
	System      string // 独立 system（适配器负责按协议摆放）
	Messages    []Message
	Tools       []ToolSpec
	Temperature *float64
	MaxTokens   int
	Stream      bool
	// ExtraBody 允许透传厂商特有字段（如 reasoning_effort）
	ExtraBody map[string]any
}

// Usage 用量（真实值来自响应；估算值由上层标记 Estimated）。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CachedTokens     int
	Estimated        bool
}

// ToolCallDelta 工具调用的流式增量（arguments 会分多片到达）。
type ToolCallDelta struct {
	Index          int
	ID             string
	Name           string
	ArgumentsDelta string
}

// Chunk 流式增量。
type Chunk struct {
	Delta        string         // 正文增量
	Reasoning    string         // 思考过程增量（部分模型支持）
	ToolCall     *ToolCallDelta // 工具调用增量
	FinishReason string         // 结束时非空：stop | tool_calls | length | content_filter
	Usage        *Usage         // 仅最后一个 chunk（stream_options.include_usage）
}

// Stream 流式读取端口。Recv 返回 io.EOF 表示正常结束。
type Stream interface {
	Recv() (Chunk, error)
	Close() error
}

// Provider 模型厂商端口。
type Provider interface {
	// Name 适配器名（日志与 provider 记录用）
	Name() string
	// ChatStream 发起流式调用。返回错误时保证未产生任何增量。
	ChatStream(ctx context.Context, req ChatRequest) (Stream, error)
}

// ErrKind 错误分类（决定上层是否重试）。
type ErrKind string

const (
	ErrKindRateLimited    ErrKind = "rate_limited" // 429：可重试
	ErrKindServerError    ErrKind = "server_error" // 5xx：可重试
	ErrKindNetwork        ErrKind = "network"      // 连接/读写失败：可重试
	ErrKindAuth           ErrKind = "auth"         // 401/403：不可重试
	ErrKindBadRequest     ErrKind = "bad_request"  // 400：不可重试
	ErrKindTimeout        ErrKind = "timeout"      // 超时：可重试
	ErrKindFirstToken     ErrKind = "first_token_too_slow"
	ErrKindContextCancel  ErrKind = "context_canceled"
	ErrKindInvalidPayload ErrKind = "invalid_payload" // 响应无法解析：不可重试
)

// Error 模型调用错误。
type Error struct {
	Kind       ErrKind
	StatusCode int
	Message    string
	RetryAfter int // 秒（来自 Retry-After 头，0 表示未知）
	cause      error
}

// Error 实现 error 接口。
func (e *Error) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("llm: %s (status=%d): %s", e.Kind, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("llm: %s: %s", e.Kind, e.Message)
}

// Unwrap 保留底层原因。
func (e *Error) Unwrap() error { return e.cause }

// Retryable 是否值得重试。
func (e *Error) Retryable() bool {
	switch e.Kind {
	case ErrKindRateLimited, ErrKindServerError, ErrKindNetwork, ErrKindTimeout, ErrKindFirstToken:
		return true
	}
	return false
}

// NewError 构造错误。
func NewError(kind ErrKind, status int, format string, args ...any) *Error {
	return &Error{Kind: kind, StatusCode: status, Message: fmt.Sprintf(format, args...)}
}

// WrapError 构造错误并保留底层原因。
func WrapError(cause error, kind ErrKind, status int, format string, args ...any) *Error {
	return &Error{Kind: kind, StatusCode: status, Message: fmt.Sprintf(format, args...), cause: cause}
}

// AsError 把任意 error 归一为 *Error（未知归为 network，保守可重试）。
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var le *Error
	if errors.As(err, &le) {
		return le
	}
	if errors.Is(err, context.Canceled) {
		return WrapError(err, ErrKindContextCancel, 0, "调用被取消")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return WrapError(err, ErrKindTimeout, 0, "调用超时")
	}
	return WrapError(err, ErrKindNetwork, 0, "模型调用失败")
}

// IsRetryable 便捷判定。
func IsRetryable(err error) bool {
	le := AsError(err)
	return le != nil && le.Retryable()
}

// EOF 便于调用方判断流结束。
var EOF = io.EOF
