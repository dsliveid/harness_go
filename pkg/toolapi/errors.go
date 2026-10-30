package toolapi

import "errors"

// 契约级哨兵错误（内核与插件共享的错误语义）。
var (
	// ErrNotFound 工具未注册。
	ErrNotFound = errors.New("toolapi: 工具未注册")
	// ErrUnsupported 本端不支持（执行面或能力不满足）。
	ErrUnsupported = errors.New("toolapi: 本端不支持该工具")
	// ErrNotImplemented 协议占位（阶段 F/G 填实）。
	ErrNotImplemented = errors.New("toolapi: 尚未实现（协议占位）")
	// ErrInvalidArgs 参数不合法。
	ErrInvalidArgs = errors.New("toolapi: 参数不合法")
	// ErrDenied 审批被拒绝。
	ErrDenied = errors.New("toolapi: 审批被拒绝")
	// ErrCancelled 调用被取消。
	ErrCancelled = errors.New("toolapi: 调用已取消")
	// ErrUnavailable 工具不可用（健康检查未通过）。
	ErrUnavailable = errors.New("toolapi: 工具不可用")
	// ErrDuplicate 工具名冲突。
	ErrDuplicate = errors.New("toolapi: 工具名冲突")
)

// IsNotImplemented 判断是否为「协议占位未实现」错误。
func IsNotImplemented(err error) bool { return errors.Is(err, ErrNotImplemented) }
