package apperr

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestNewAndError(t *testing.T) {
	e := New(CodeAuthInvalidCredentials, "凭据无效")
	if e.Code != CodeAuthInvalidCredentials {
		t.Fatalf("错误码错误: %s", e.Code)
	}
	if !strings.Contains(e.Error(), string(CodeAuthInvalidCredentials)) {
		t.Fatalf("Error() 应含错误码: %s", e.Error())
	}
	if e.Status() != http.StatusUnauthorized {
		t.Fatalf("状态码应为 401，得到 %d", e.Status())
	}
}

func TestWrapKeepsCause(t *testing.T) {
	inner := errors.New("连接超时")
	e := Wrap(inner, CodeUnavailable, "上游不可用")
	if !errors.Is(e, inner) {
		t.Fatal("Wrap 应保留内部原因")
	}
	// 对外响应体不得泄露内部原因
	b := e.Body()
	if strings.Contains(b.Error.Message, "连接超时") {
		t.Fatalf("响应体泄露内部原因: %+v", b.Error)
	}
}

func TestFromNormalizesUnknownError(t *testing.T) {
	raw := errors.New("sql: database is locked")
	e := From(raw)
	if e.Code != CodeInternal {
		t.Fatalf("未知错误应归为 internal，得到 %s", e.Code)
	}
	if strings.Contains(e.Message, "database is locked") {
		t.Fatalf("消息不应泄露内部细节: %s", e.Message)
	}
	if !errors.Is(e, raw) {
		t.Fatal("应保留原始错误供日志使用")
	}
}

func TestFromPassesThroughAppErr(t *testing.T) {
	orig := New(CodeRunNotFound, "运行不存在")
	got := From(orig)
	if got != orig {
		t.Fatal("已知 *Error 应原样返回")
	}
	if From(nil) != nil {
		t.Fatal("nil 应返回 nil")
	}
}

func TestStatusForMapping(t *testing.T) {
	cases := map[Code]int{
		CodeInvalidArgument:        http.StatusBadRequest,
		CodeToolBadArgs:            http.StatusBadRequest,
		CodeUnauthorized:           http.StatusUnauthorized,
		CodeAuthSignExpired:        http.StatusUnauthorized,
		CodeAuthSSODisabled:        http.StatusForbidden,
		CodeToolDenied:             http.StatusForbidden,
		CodeSessionNotFound:        http.StatusNotFound,
		CodeRunStateConflict:       http.StatusConflict,
		CodeAuthRateLimited:        http.StatusTooManyRequests,
		CodeStorageBlobTooLarge:    http.StatusRequestEntityTooLarge,
		CodeRunConcurrencyFull:     http.StatusServiceUnavailable,
		CodeStorageMigrationFailed: http.StatusInternalServerError,
	}
	for code, want := range cases {
		if got := StatusFor(code); got != want {
			t.Fatalf("%s 状态码期望 %d，得到 %d", code, want, got)
		}
	}
}

func TestBodyJSONShape(t *testing.T) {
	e := New(CodeStorageBlobTooLarge, "文件超过 %dMB 上限", 64).
		WithTrace("abcdef0123456789").
		WithDetails(map[string]any{"max_mb": 64})

	raw, err := json.Marshal(e.Body())
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			TraceID string         `json:"trace_id"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Error.Code != string(CodeStorageBlobTooLarge) {
		t.Fatalf("code 字段错误: %s", decoded.Error.Code)
	}
	if decoded.Error.TraceID != "abcdef0123456789" {
		t.Fatalf("trace_id 未透传: %s", decoded.Error.TraceID)
	}
	if decoded.Error.Details["max_mb"] != float64(64) {
		t.Fatalf("details 未透传: %+v", decoded.Error.Details)
	}
}

func TestWithStatusOverride(t *testing.T) {
	e := New(CodeConflict, "已存在").WithStatus(http.StatusAccepted)
	if e.Status() != http.StatusAccepted {
		t.Fatalf("覆盖状态码失败: %d", e.Status())
	}
}
