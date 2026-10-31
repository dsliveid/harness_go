// Package apperr 定义统一错误模型：错误码 + 可读消息 + trace_id + HTTP 状态映射。
//
// 设计约束（见 docs/架构设计文档.md §10.1）：
//   - 对外只暴露稳定的字符串错误码（前端与外部接入方据此分支），不暴露内部错误链
//   - 内部原因通过 Unwrap 保留，仅供日志与排障
//   - 所有 HTTP 错误响应体统一为 {"error":{code,message,trace_id,details}}
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

// Code 稳定错误码（对外契约的一部分，禁止随意改名）。
type Code string

// 通用
const (
	CodeInternal        Code = "internal"
	CodeInvalidArgument Code = "invalid_argument"
	CodeNotFound        Code = "not_found"
	CodeConflict        Code = "conflict"
	CodeRateLimited     Code = "rate_limited"
	CodeUnauthorized    Code = "unauthorized"
	CodeForbidden       Code = "forbidden"
	CodeUnavailable     Code = "unavailable"
)

// 认证与免登（docs/开发实施计划.md 附录 A）
const (
	CodeAuthInvalidCredentials Code = "auth.invalid_credentials"
	CodeAuthSSODisabled        Code = "auth.sso.disabled"
	CodeAuthRateLimited        Code = "auth.rate_limited"
	CodeAuthSignExpired        Code = "auth.sign_expired"
	CodeAuthSessionExpired     Code = "auth.session_expired"
	CodeAuthPasswordPolicy     Code = "auth.password_policy"
)

// 存储（docs/数据存储方案.md）
const (
	CodeStorageBlobTooLarge      Code = "storage.blob_too_large"
	CodeStorageCrossDevice       Code = "storage.cross_device"
	CodeStorageMigrationFailed   Code = "storage.migration_failed"
	CodeStorageIntegrityFailed   Code = "storage.integrity_failed"
	CodeStorageSessionPoolClosed Code = "storage.session_pool_closed"
)

// 会话与运行
const (
	CodeSessionNotFound    Code = "session.not_found"
	CodeRunNotFound        Code = "run.not_found"
	CodeRunStateConflict   Code = "run.state_conflict"
	CodeRunInterrupted     Code = "run.interrupted"
	CodeRunConcurrencyFull Code = "run.concurrency_full"
)

// 工具与插件
const (
	CodeToolNotFound    Code = "tool.not_found"
	CodeToolDenied      Code = "tool.denied"
	CodeToolTimeout     Code = "tool.timeout"
	CodeToolBadArgs     Code = "tool.bad_args"
	CodeToolUnavailable Code = "tool.unavailable"
	CodePluginDegraded  Code = "plugin.degraded"
)

// 审批（B-3：审批决策端点 / C-7：跨端仲裁）
const (
	CodeApprovalNotFound      Code = "approval.not_found"
	CodeApprovalAlreadyDecide Code = "approval.already_decided"
	// CodeApprovalDeviceMismatch 审批绑定了执行端设备，决策方不是该设备（strict 模式）
	CodeApprovalDeviceMismatch Code = "approval.device_mismatch"
)

// 设备与多端协同（阶段 C / 多端协同 HALO M5）
const (
	// CodeDevicePairCodeInvalid 配对码不存在 / 已消费 / 已过期（三者统一，防枚举）
	CodeDevicePairCodeInvalid Code = "device.pair_code_invalid"
	// CodeDeviceNotFound 设备不存在
	CodeDeviceNotFound Code = "device.not_found"
	// CodeDeviceRevoked 设备已吊销（令牌立即失效）
	CodeDeviceRevoked Code = "device.revoked"
	// CodeDeviceProtocolMismatch 端侧协议版本不兼容（支持 N / N-1）
	CodeDeviceProtocolMismatch Code = "device.protocol_mismatch"
	// CodeDeviceDisabled 设备域被配置关闭
	CodeDeviceDisabled Code = "device.disabled"
)

// Error 统一错误类型。
type Error struct {
	Code    Code
	Message string
	TraceID string
	Details map[string]any
	status  int
	cause   error
}

// Error 实现 error 接口，输出便于日志阅读的形式。
func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap 保留内部原因，仅供日志与 errors.Is/As 使用。
func (e *Error) Unwrap() error { return e.cause }

// New 构造一个错误。
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), status: StatusFor(code)}
}

// Wrap 在保留内部原因的前提下构造错误。
func Wrap(cause error, code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), status: StatusFor(code), cause: cause}
}

// WithTrace 附加 trace_id（由中间件在写出响应前统一注入）。
func (e *Error) WithTrace(traceID string) *Error {
	e.TraceID = traceID
	return e
}

// WithDetails 附加结构化细节（仅放对用户有用的信息，禁放内部堆栈）。
func (e *Error) WithDetails(kv map[string]any) *Error {
	if e.Details == nil {
		e.Details = make(map[string]any, len(kv))
	}
	for k, v := range kv {
		e.Details[k] = v
	}
	return e
}

// WithStatus 覆盖默认 HTTP 状态码（少数场景需要，如 202 Accepted 语义）。
func (e *Error) WithStatus(status int) *Error {
	e.status = status
	return e
}

// Status 返回应使用的 HTTP 状态码。
func (e *Error) Status() int {
	if e.status != 0 {
		return e.status
	}
	return StatusFor(e.Code)
}

// Info 是响应体中 error 字段的结构。
type Info struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	TraceID string         `json:"trace_id,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// Body 是统一错误响应体。
type Body struct {
	Error Info `json:"error"`
}

// Body 生成对外响应体（剥离内部原因）。
func (e *Error) Body() Body {
	return Body{Error: Info{Code: string(e.Code), Message: e.Message, TraceID: e.TraceID, Details: e.Details}}
}

// From 把任意 error 归一为 *Error。
// 未知错误统一归为 internal，且消息固定为通用文案（避免泄露内部细节）。
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var ae *Error
	if errors.As(err, &ae) {
		return ae
	}
	return &Error{
		Code:    CodeInternal,
		Message: "服务内部错误，请携带 trace_id 反馈",
		status:  http.StatusInternalServerError,
		cause:   err,
	}
}

// StatusFor 错误码 → HTTP 状态码的默认映射。
func StatusFor(code Code) int {
	switch code {
	case CodeInvalidArgument, CodeToolBadArgs, CodeAuthPasswordPolicy:
		return http.StatusBadRequest
	case CodeUnauthorized, CodeAuthInvalidCredentials, CodeAuthSignExpired,
		CodeAuthSessionExpired, CodeDevicePairCodeInvalid, CodeDeviceRevoked:
		return http.StatusUnauthorized
	case CodeForbidden, CodeAuthSSODisabled, CodeToolDenied, CodeApprovalDeviceMismatch:
		return http.StatusForbidden
	case CodeNotFound, CodeSessionNotFound, CodeRunNotFound, CodeToolNotFound,
		CodeApprovalNotFound, CodeDeviceNotFound:
		return http.StatusNotFound
	case CodeConflict, CodeRunStateConflict, CodeApprovalAlreadyDecide,
		CodeDeviceProtocolMismatch:
		return http.StatusConflict
	case CodeRateLimited, CodeAuthRateLimited:
		return http.StatusTooManyRequests
	case CodeStorageBlobTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeUnavailable, CodeToolUnavailable, CodePluginDegraded,
		CodeStorageSessionPoolClosed, CodeRunConcurrencyFull, CodeDeviceDisabled:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
