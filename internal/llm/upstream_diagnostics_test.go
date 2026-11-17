package llm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ============ 上游失败现场留档（bad_request 可诊断性）契约 ============
//
// 背景（真实故障）：前端只显示「bad_request: 请求被上游拒绝」，
// 而服务端把上游响应体 `io.Copy(io.Discard, ...)` 丢弃，导致
// 「参数非法 / 上下文超限 / 模型不存在 / 消息序列缺配对 tool 结果」
// 这四类天差地别的真实原因在日志与审计库里全都查不到 —— 排障死胡同。
//
// 本文件锁定三条不变量：
//  1. 上游原因必须出现在 Error.Message 里（前端能直接看到人话原因）；
//  2. 上游原文必须留档在 Error.UpstreamBody（供结构化日志/审计，且要截断）；
//  3. 留档改造**不得**破坏原有分类与重试语义（400 仍不可重试、仍只请求一次）。

// TestUpstreamReasonPropagatedIntoMessage 上游 OpenAI 形状的 error.message 必须进文案。
func TestUpstreamReasonPropagatedIntoMessage(t *testing.T) {
	srv := rawSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"This model's maximum context length is 128000 tokens","type":"invalid_request_error","code":"context_length_exceeded"}}`)
	})
	p := newTestProvider(t, srv.URL, 5*time.Second)

	_, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Stream: true})
	if err == nil {
		t.Fatal("400 必须返回错误")
	}
	le := AsError(err)
	if le.Kind != ErrKindBadRequest || le.StatusCode != http.StatusBadRequest {
		t.Fatalf("分类/状态码被破坏: kind=%s status=%d", le.Kind, le.StatusCode)
	}
	// ① 人话文案里必须带上游原话，否则用户依旧只看到「请求被上游拒绝」
	if !strings.Contains(le.Message, "maximum context length") {
		t.Fatalf("消息必须包含上游原因，得到: %s", le.Message)
	}
	if !strings.Contains(le.Message, "请求被上游拒绝") {
		t.Fatalf("分类前缀必须保留，得到: %s", le.Message)
	}
	// ② 原文必须留档
	if !strings.Contains(le.UpstreamBody, "context_length_exceeded") {
		t.Fatalf("UpstreamBody 必须保留上游原文，得到: %s", le.UpstreamBody)
	}
}

// TestUpstreamReasonAnthropicShape 兼容 Anthropic 的 {"type":"error","error":{...}} 形状。
func TestUpstreamReasonAnthropicShape(t *testing.T) {
	raw := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: must be greater than 0"}}`)
	got := upstreamReason(raw)
	if !strings.Contains(got, "invalid_request_error") || !strings.Contains(got, "max_tokens") {
		t.Fatalf("Anthropic 形状未解析出 type+message，得到: %q", got)
	}
}

// TestUpstreamReasonGatewayShape 兼容网关/FastAPI 的 message / detail 平铺形状。
func TestUpstreamReasonGatewayShape(t *testing.T) {
	if got := upstreamReason([]byte(`{"message":"no healthy upstream"}`)); got != "no healthy upstream" {
		t.Fatalf("message 平铺形状未解析，得到: %q", got)
	}
	if got := upstreamReason([]byte(`{"detail":"Not Found"}`)); got != "Not Found" {
		t.Fatalf("detail 平铺形状未解析，得到: %q", got)
	}
}

// TestUpstreamReasonNonJSONNotPolluted 非 JSON（HTML 错误页/纯文本）不得污染文案，只留档原文。
//
// 为什么要这条：网关 502 常回一大段 HTML，直接拼进 error_message 会把
// 前端提示条撑爆并淹没真正的分类信息。
func TestUpstreamReasonNonJSONNotPolluted(t *testing.T) {
	srv := rawSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway - 100) // 502 → 402 走 >=400 分支，同样不可重试
		_, _ = io.WriteString(w, `<html><body>502 Bad Gateway</body></html>`)
	})
	p := newTestProvider(t, srv.URL, 5*time.Second)

	_, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Stream: true})
	if err == nil {
		t.Fatal("4xx 必须返回错误")
	}
	le := AsError(err)
	if strings.Contains(le.Message, "<html>") {
		t.Fatalf("HTML 不应进人话文案，得到: %s", le.Message)
	}
	if !strings.Contains(le.UpstreamBody, "502 Bad Gateway") {
		t.Fatalf("原文仍须留档，得到: %s", le.UpstreamBody)
	}
}

// TestUpstreamBodyTruncated 超长报文必须截断，避免把日志/审计库撑爆。
func TestUpstreamBodyTruncated(t *testing.T) {
	huge := strings.Repeat("长", maxUpstreamReasonRunes*2)
	raw := []byte(`{"error":{"message":"` + huge + `"}}`)
	le := withUpstreamMessage(NewError(ErrKindBadRequest, 400, "请求被上游拒绝"), raw)

	if n := len([]rune(le.UpstreamBody)); n > maxUpstreamReasonRunes+1 {
		t.Fatalf("UpstreamBody 未截断，runes=%d 上限=%d", n, maxUpstreamReasonRunes)
	}
	// 截断不得产生非法 UTF-8（按字符截断而非字节）
	if !strings.Contains(le.Message, "请求被上游拒绝") {
		t.Fatalf("分类前缀丢失: %s", le.Message)
	}
	if strings.ToValidUTF8(le.Message, "") != le.Message {
		t.Fatalf("截断后消息含非法 UTF-8: %s", le.Message)
	}
}

// TestUpstreamReasonEmptyBodyFallsBack 空响应体不应崩、不应污染文案。
func TestUpstreamReasonEmptyBodyFallsBack(t *testing.T) {
	le := withUpstreamMessage(NewError(ErrKindBadRequest, 400, "请求被上游拒绝"), nil)
	if le.Message != "请求被上游拒绝" {
		t.Fatalf("空报文不应改动文案，得到: %s", le.Message)
	}
	if le.UpstreamBody != "" {
		t.Fatalf("空报文 UpstreamBody 应为空，得到: %q", le.UpstreamBody)
	}
}

// TestRetrySemanticsUnchangedByDiagnostics 留档改造后，可重试分级必须原样保持。
//
// 这是回归保护：一旦有人为了「多拿点日志」把 5xx 改成不可重试、
// 或把 400 改成可重试，都会在这里炸出来（后者会放大重复计费）。
func TestRetrySemanticsUnchangedByDiagnostics(t *testing.T) {
	cases := []struct {
		status    int
		kind      ErrKind
		retryable bool
	}{
		{http.StatusBadRequest, ErrKindBadRequest, false},
		{http.StatusUnauthorized, ErrKindAuth, false},
		{http.StatusForbidden, ErrKindAuth, false},
		{http.StatusNotFound, ErrKindBadRequest, false},
		{http.StatusTooManyRequests, ErrKindRateLimited, true},
		{http.StatusInternalServerError, ErrKindServerError, true},
	}
	for _, tc := range cases {
		var calls int32
		srv := rawSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
		})
		p := newTestProvider(t, srv.URL, 5*time.Second)
		_, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Stream: true})
		le := AsError(err)
		if le == nil {
			t.Fatalf("status=%d 必须返回错误", tc.status)
		}
		if le.Kind != tc.kind {
			t.Fatalf("status=%d 期望 kind=%s，得到 %s", tc.status, tc.kind, le.Kind)
		}
		if le.Retryable() != tc.retryable {
			t.Fatalf("status=%d 期望 retryable=%v，得到 %v", tc.status, tc.retryable, le.Retryable())
		}
		// 可重试的会重试 maxRetries 次（初始 1 次 + 2 次重试 = 3）
		wantCalls := int32(1)
		if tc.retryable {
			wantCalls = 3
		}
		if got := atomic.LoadInt32(&calls); got != wantCalls {
			t.Fatalf("status=%d 期望请求 %d 次，实际 %d", tc.status, wantCalls, got)
		}
	}
}

// TestListModelsErrorKeepsUpstreamReason 模型清单拉取同样必须保留上游原因。
//
// 这条链路不经过 agent，出错时前端只有一个聚合文案，
// 若这里不记，模型列表拉不到就彻底无从排查。
func TestListModelsErrorKeepsUpstreamReason(t *testing.T) {
	srv := rawSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key provided"}}`)
	})
	p := newTestProvider(t, srv.URL, 5*time.Second)

	_, err := p.ListModels(context.Background())
	if err == nil {
		t.Fatal("401 必须返回错误")
	}
	le := AsError(err)
	if !strings.Contains(le.Message, "invalid api key") {
		t.Fatalf("清单拉取的错误也必须带上游原因，得到: %s", le.Message)
	}
}
