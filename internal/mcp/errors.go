package mcp

import "fmt"

// 归一错误码（docs/MCP接入设计.md §7）。
//
// 关键约束：工具级错误作为工具结果回传模型（role=tool，不中断 Run）；
// 只有连接级故障才把该 Server 全部工具移出注入集合。
const (
	// ErrCodeConnectFailed 连接/握手失败 → Server 置 failed。
	ErrCodeConnectFailed = "MCP_CONNECT_FAILED"
	// ErrCodeTimeout 工具调用超时（不自动重试——可能有副作用）。
	ErrCodeTimeout = "MCP_TIMEOUT"
	// ErrCodeToolError 工具返回 isError（模型可自我纠正）。
	ErrCodeToolError = "MCP_TOOL_ERROR"
	// ErrCodeProcessExit 进程崩溃 → degraded + 尝试重建。
	ErrCodeProcessExit = "MCP_PROCESS_EXIT"
	// ErrCodeProtocolError 协议解析错误。
	ErrCodeProtocolError = "MCP_PROTOCOL_ERROR"
	// ErrCodeDisabled Server 已禁用/未启动。
	ErrCodeDisabled = "MCP_DISABLED"
)

// Error 归一的 MCP 错误（携带错误码，供 API 与工具结果使用）。
type Error struct {
	Code    string
	Server  string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("mcp: [%s] server=%s: %s", e.Code, e.Server, e.Message)
}

// newError 构造归一错误。
func newError(code, server, format string, args ...any) *Error {
	return &Error{Code: code, Server: server, Message: fmt.Sprintf(format, args...)}
}
