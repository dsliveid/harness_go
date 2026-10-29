package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// sseServer 构造假 SSE 服务端：按 chunks 顺序写出，最后发 [DONE]。
func sseServer(t *testing.T, chunks []string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, c := range chunks {
			if _, err := io.WriteString(w, "data: "+c+"\n\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func newTestProvider(t *testing.T, baseURL string, firstToken time.Duration) *OpenAICompat {
	t.Helper()
	p, err := NewOpenAICompat(OpenAICompatConfig{
		Name:              "test",
		BaseURL:           baseURL,
		APIKey:            "sk-test",
		MaxRetries:        2,
		BaseDelay:         time.Millisecond,
		FirstTokenTimeout: firstToken,
	})
	if err != nil {
		t.Fatalf("创建 provider 失败: %v", err)
	}
	return p
}

func TestChatStreamNormalFlow(t *testing.T) {
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"你好"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"，世界"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}}`,
	}
	srv, calls := sseServer(t, chunks)
	p := newTestProvider(t, srv.URL, 5*time.Second)

	ctx := context.Background()
	stream, err := p.ChatStream(ctx, ChatRequest{Model: "gpt-4o", Stream: true})
	if err != nil {
		t.Fatalf("ChatStream 失败: %v", err)
	}
	defer stream.Close()

	var text string
	var finish string
	var usage *Usage
	for {
		ch, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv 失败: %v", err)
		}
		text += ch.Delta
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
		if ch.Usage != nil {
			usage = ch.Usage
		}
	}
	if text != "你好，世界" {
		t.Fatalf("正文拼接错误: %q", text)
	}
	if finish != "stop" {
		t.Fatalf("finish_reason 应为 stop，得到 %q", finish)
	}
	if usage == nil {
		t.Fatal("未解析到 usage")
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.TotalTokens != 15 {
		t.Fatalf("usage 错误: %+v", usage)
	}
	if usage.CachedTokens != 4 {
		t.Fatalf("cached_tokens 应为 4，得到 %d", usage.CachedTokens)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("应只请求 1 次，实际 %d", got)
	}
}

func TestChatStreamToolCalls(t *testing.T) {
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	srv, _ := sseServer(t, chunks)
	p := newTestProvider(t, srv.URL, 5*time.Second)

	stream, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var id, name, args string
	for {
		ch, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ch.ToolCall != nil {
			if ch.ToolCall.ID != "" {
				id = ch.ToolCall.ID
			}
			if ch.ToolCall.Name != "" {
				name = ch.ToolCall.Name
			}
			args += ch.ToolCall.ArgumentsDelta
		}
	}
	if id != "call_1" || name != "read_file" {
		t.Fatalf("工具调用标识错误: id=%q name=%q", id, name)
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("参数分片拼接后不是合法 JSON: %q", args)
	}
	if parsed["path"] != "a.txt" {
		t.Fatalf("参数解析错误: %+v", parsed)
	}
}

// 429 应重试并在第 2 次成功。
func TestRetryOn429(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL, 5*time.Second)
	stream, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("429 后应重试成功，得到错误: %v", err)
	}
	defer stream.Close()
	ch, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if ch.Delta != "ok" {
		t.Fatalf("重试后内容错误: %q", ch.Delta)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("应请求 2 次，实际 %d", got)
	}
}

// 401 不可重试，应立即失败。
func TestNoRetryOn401(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL, 5*time.Second)
	_, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Stream: true})
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	le := AsError(err)
	if le.Kind != ErrKindAuth {
		t.Fatalf("错误类型应为 auth，得到 %s", le.Kind)
	}
	if le.Retryable() {
		t.Fatal("401 不应可重试")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("不应重试，实际请求 %d 次", got)
	}
}

// 5xx 重试耗尽后返回 server_error。
func TestRetryExhausted(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL, 5*time.Second)
	_, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Stream: true})
	if err == nil {
		t.Fatal("应返回错误")
	}
	if le := AsError(err); le.Kind != ErrKindServerError {
		t.Fatalf("错误类型应为 server_error，得到 %s", le.Kind)
	}
	// maxRetries=2 → 共 3 次
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("应请求 3 次，实际 %d", got)
	}
}

// 首 token 超时：服务端迟迟不返回数据。
func TestFirstTokenTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(600 * time.Millisecond) // 超过首 token 超时
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"late\"}}]}\n\n")
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL, 120*time.Millisecond)
	stream, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("ChatStream 本身应成功（连接已建立）: %v", err)
	}
	defer stream.Close()

	_, err = stream.Recv()
	if err == nil {
		t.Fatal("应返回首 token 超时错误")
	}
	if le := AsError(err); le.Kind != ErrKindFirstToken {
		t.Fatalf("错误类型应为 first_token_too_slow，得到 %s", le.Kind)
	}
}

// 请求体必须带 stream_options.include_usage 与 tools。
func TestBuildBodyShape(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL, 5*time.Second)
	temp := 0.3
	stream, err := p.ChatStream(context.Background(), ChatRequest{
		Model:       "gpt-4o",
		System:      "你是助手",
		Messages:    []Message{{Role: RoleUser, Content: "hi"}},
		Tools:       []ToolSpec{{Name: "echo", Description: "回显", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Temperature: &temp,
		MaxTokens:   100,
		Stream:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	if captured["model"] != "gpt-4o" {
		t.Fatalf("model 未透传: %v", captured["model"])
	}
	so, ok := captured["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Fatalf("缺少 stream_options.include_usage: %v", captured["stream_options"])
	}
	tools, ok := captured["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools 未透传: %v", captured["tools"])
	}
	msgs, ok := captured["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("system 应作为首条消息: %v", captured["messages"])
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "你是助手" {
		t.Fatalf("system 消息错误: %v", first)
	}
	if captured["temperature"] != 0.3 {
		t.Fatalf("temperature 未透传: %v", captured["temperature"])
	}
	if captured["max_tokens"] != float64(100) {
		t.Fatalf("max_tokens 未透传: %v", captured["max_tokens"])
	}
}

// 心跳/注释/空 chunk 不应打断解析。
func TestSkipHeartbeatAndEmptyChunks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": keep-alive\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\n") // 空 choices
		_, _ = io.WriteString(w, "data: not-json\n\n")         // 脏数据
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL, 5*time.Second)
	stream, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	ch, err := stream.Recv()
	if err != nil {
		t.Fatalf("应跳过心跳与脏数据: %v", err)
	}
	if ch.Delta != "ok" {
		t.Fatalf("内容错误: %q", ch.Delta)
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("结束应为 io.EOF，得到 %v", err)
	}
}

// 上下文取消应可中断请求。
func TestContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL, 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	stream, err := p.ChatStream(ctx, ChatRequest{Model: "m", Stream: true})
	if err != nil {
		// 建立阶段就被取消也属于正确行为
		return
	}
	defer stream.Close()
	if _, err := stream.Recv(); err == nil {
		t.Fatal("取消后 Recv 应返回错误")
	}
}

func TestNewOpenAICompatRejectsEmptyBaseURL(t *testing.T) {
	if _, err := NewOpenAICompat(OpenAICompatConfig{BaseURL: "  "}); err == nil {
		t.Fatal("空 BaseURL 应报错")
	}
}

func TestErrorRetryableClassification(t *testing.T) {
	cases := map[ErrKind]bool{
		ErrKindRateLimited:    true,
		ErrKindServerError:    true,
		ErrKindNetwork:        true,
		ErrKindTimeout:        true,
		ErrKindFirstToken:     true,
		ErrKindAuth:           false,
		ErrKindBadRequest:     false,
		ErrKindInvalidPayload: false,
		ErrKindContextCancel:  false,
	}
	for kind, want := range cases {
		e := NewError(kind, 0, "x")
		if e.Retryable() != want {
			t.Fatalf("%s 可重试应为 %v", kind, want)
		}
	}
	if !IsRetryable(fmt.Errorf("plain error")) {
		t.Fatal("未知错误保守视为可重试")
	}
}
