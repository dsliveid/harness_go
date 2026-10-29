package service

import (
	"errors"

	"harness-go/internal/apperr"
	"harness-go/internal/store"
)

// wrapStoreErr 把 store 的哨兵错误翻译成对外业务错误码。
//
// 为什么在 service 层做：只有用例层知道「这个 not_found 是会话还是 Run」，
// 而 store 只知道自己没查到行。api 层因此不需要了解任何存储细节。
func wrapStoreErr(err error, code apperr.Code, msg string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return apperr.Wrap(err, code, "%s", msg)
	}
	return err
}

// invalidArg 构造参数错误（400）。
func invalidArg(format string, args ...any) error {
	return apperr.New(apperr.CodeInvalidArgument, format, args...)
}
