package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ============ Tier-1 证据：LLM 离线契约评测集 ============
//
// 设计口径（对齐 docs/架构设计文档.md §2.3「适配器只做请求 → 流式增量翻译」）：
//   - 全部基于 httptest 本地假 SSE 服务端，**零外网、零额度**，可离线重跑
//   - 断言聚焦「可观测行为」：拼接后的完整正文 / 完整工具参数 / usage 数值 / 终态与错误分类
//   - 不复用任何业务实现内部状态，只走 ChatStream + Stream.Recv 公共端口
//
// 与既有 openai_compat_test.go 的分工：既有文件覆盖正常流、重试、鉴权、超时等主干；
// 本文件补充 [DONE] 收尾语义、无 [DONE] 优雅收尾、传输层中断、流内错误帧、
// 多片工具参数严格拼接、usage 缺失不伪造、reasoning 增量、工具消息往返等边界契约。
// 既有测试一律不改写，只新增 Test 函数。

// rawSSEServer 自定义 SSE 服务端：由 handler 自行决定写多少 / 是否发 [DONE]，
// 便于构造「中断」「错误帧」等异常收尾场景。
func rawSSEServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// drainStream 读完整个流，返回拼接正文、拼接工具参数、最后一个 usage 与收到的增量条数。
func drainStream(t *testing.T, s Stream) (text string, args string, toolName string, toolID string, usage *Usage, n int) {
	t.Helper()
	for {
		ch, err := s.Recv()
		if err == io.EOF {
			return text, args, toolName, toolID, usage, n
		}
		if err != nil {
			t.Fatalf("Recv 非预期错误: %v", err)
		}
		n++
		text += ch.Delta
		if ch.Usage != nil {
			usage = ch.Usage
		}
		if ch.ToolCall != nil {
			if ch.ToolCall.ID != "" {
				toolID = ch.ToolCall.ID
			}
			if ch.ToolCall.Name != "" {
				toolName = ch.ToolCall.Name
			}
			args += ch.ToolCall.ArgumentsDelta
		}
	}
}

// TestContractDoneTerminatesStreamAndCloseIsIdempotent
// 不变量：收到 [DONE] 后 Recv 必须恒返回 io.EOF（而不是阻塞 / 返回脏数据），
//
//	且 Close 幂等（重复关闭不 panic、不返回错误）。
func TestContractDoneTerminatesStreamAndCloseIsIdempotent(t *testing.T) {
	srv, _ := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"A"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"B"}}]}`,
	})
	p := newTestProvider(t, srv.URL, 5*time.Second)

	stream, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("ChatStream 失败: %v", err)
	}

	text, _, _, _, _, n := drainStream(t, stream)
	if text != "AB" {
		t.Fatalf("[DONE] 之前的两片正文应完整拼接为 %q，得到 %q", "AB", text)
	}
	if n != 2 {
		t.Fatalf("应收到 2 个有效增量（[DONE] 不计入），得到 %d", n)
	}

	// [DONE] 之后再次 Recv：必须是 io.EOF，不能返回内容或错误
	for i := 0; i < 3; i++ {
		ch, rErr := stream.Recv()
		if rErr != io.EOF {
			t.Fatalf("第 %d 次重复 Recv 应返回 io.EOF，得到 err=%v chunk=%+v", i+1, rErr, ch)
		}
		if ch.Delta != "" || ch.Usage != nil || ch.ToolCall != nil {
			t.Fatalf("流终止后不得再产出增量，得到 %+v", ch)
		}
	}

	// Close 幂等
	if cErr := stream.Close(); cErr != nil {
		t.Fatalf("首次 Close 不应报错: %v", cErr)
	}
	if cErr := stream.Close(); cErr != nil {
		t.Fatalf("重复 Close 必须幂等，得到错误: %v", cErr)
	}
}

// TestContractStreamEndsWithoutDoneGracefully
// 不变量：上游未发 [DONE] 但连接正常关闭时，已收到的增量必须全部保留，
//
//	Recv 以 io.EOF 收敛（不得因缺 [DONE] 就报错丢弃内容）。
func TestContractStreamEndsWithoutDoneGracefully(t *testing.T) {
	srv := rawSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"半\"}}]}\n\n")
		if f != nil {
			f.Flush()
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"截\"}}]}\n\n")
		if f != nil {
			f.Flush()
		}
		// 故意不发 [DONE]，直接返回（HTTP body EOF）
	})
	p := newTestProvider(t, srv.URL, 5*time.Second)

	stream, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("ChatStream 失败: %v", err)
	}
	defer stream.Close()

	text, _, _, _, _, n := drainStream(t, stream)
	if n != 2 {
		t.Fatalf("缺 [DONE] 不应丢增量，应收到 2 个，得到 %d", n)
	}
	if text != "半截" {
		t.Fatalf("缺 [DONE] 时已收正文必须完整保留，得到 %q", text)
	}
}

// TestContractStreamTransportBreakIsNetworkError
// 不变量：传输层截断（Content-Length 声明大于实际字节）必须归类为
//
//	ErrKindNetwork 且可重试，不得被当作正常 EOF 静默吞掉。
func TestContractStreamTransportBreakIsNetworkError(t *testing.T) {
	srv := rawSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 声明一个远大于实际写入的 Content-Length，制造 unexpected EOF
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 写完就返回，实际字节 << 声明值
	})
	p := newTestProvider(t, srv.URL, 5*time.Second)

	stream, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("ChatStream 本身应成功（连接已建立）: %v", err)
	}
	defer stream.Close()

	var sawDelta bool
	var gotErr error
	for i := 0; i < 8; i++ {
		ch, rErr := stream.Recv()
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			gotErr = rErr
			break
		}
		if ch.Delta == "x" {
			sawDelta = true
		}
	}
	if gotErr == nil {
		t.Fatal("传输层截断必须返回错误，不得静默当作正常 EOF")
	}
	le := AsError(gotErr)
	if le.Kind != ErrKindNetwork {
		t.Fatalf("截断应归类为 network，得到 %s（%v）", le.Kind, gotErr)
	}
	if !le.Retryable() {
		t.Fatalf("network 错误应可重试，得到 Retryable=false：%v", gotErr)
	}
	if !sawDelta {
		t.Fatal("截断前已收到的增量应正常产出，不应被错误吞掉")
	}
}

// TestContractStreamErrorFrameMidStream
// 不变量：SSE data 内携带 {"error":{...}} 时，Recv 立即返回 ErrKindServerError
//
//	并且错误信息包含上游 message（便于用户定位「模型不存在」类问题）。
func TestContractStreamErrorFrameMidStream(t *testing.T) {
	srv, _ := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"前"}}]}`,
		`{"error":{"message":"model_not_found: gpt-9","type":"invalid_request_error"}}`,
	})
	p := newTestProvider(t, srv.URL, 5*time.Second)

	stream, err := p.ChatStream(context.Background(), ChatRequest{Model: "gpt-9", Stream: true})
	if err != nil {
		t.Fatalf("ChatStream 失败: %v", err)
	}
	defer stream.Close()

	ch, err := stream.Recv()
	if err != nil {
		t.Fatalf("错误帧之前的内容应正常产出: %v", err)
	}
	if ch.Delta != "前" {
		t.Fatalf("首片正文应为 %q，得到 %q", "前", ch.Delta)
	}

	_, err = stream.Recv()
	if err == nil {
		t.Fatal("错误帧必须让 Recv 返回错误")
	}
	le := AsError(err)
	if le.Kind != ErrKindServerError {
		t.Fatalf("流内错误帧应归类为 server_error，得到 %s", le.Kind)
	}
	if !strings.Contains(le.Message, "model_not_found: gpt-9") {
		t.Fatalf("错误信息应透传上游 message，得到 %q", le.Message)
	}
}

// TestContractToolCallArgumentsAssembledAcrossManyChunks
// 不变量：tool_calls.arguments 被拆成任意多片时，按到达顺序拼接必须得到
//
//	**逐字节相等**的原始 JSON 参数串，且可被 json.Unmarshal 解析。
func TestContractToolCallArgumentsAssembledAcrossManyChunks(t *testing.T) {
	wantArgs := `{"path":"deep/dir/file.txt","max_bytes":4096}`
	// 把参数串切成 7 片，刻意在 JSON 语法单元中间切开
	cuts := []string{`{"pa`, `th":"deep/d`, `ir/file.tx`, `t","max_by`, `tes":`, `4096`, `}`}
	if joined := strings.Join(cuts, ""); joined != wantArgs {
		t.Fatalf("测试夹具自身不一致: %q != %q", joined, wantArgs)
	}

	frames := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"fs.read","arguments":""}}]}}]}`,
	}
	for i, part := range cuts {
		if i == 0 {
			// 首片参数与 name 同行到达（真实上游常见形状）
			frames = append(frames, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":`+
				mustJSONString(part)+`}}]}}]}`)
			continue
		}
		frames = append(frames, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":`+
			mustJSONString(part)+`}}]}}]}`)
	}
	frames = append(frames, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)

	srv, _ := sseServer(t, frames)
	p := newTestProvider(t, srv.URL, 5*time.Second)

	stream, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("ChatStream 失败: %v", err)
	}
	defer stream.Close()

	text, args, toolName, toolID, _, _ := drainStream(t, stream)
	if text != "" {
		t.Fatalf("纯工具调用轮次不应产出正文，得到 %q", text)
	}
	if toolID != "call_abc" {
		t.Fatalf("工具调用 ID 应从首片解析出来，得到 %q", toolID)
	}
	if toolName != "fs.read" {
		t.Fatalf("工具名解析错误: %q", toolName)
	}
	if args != wantArgs {
		t.Fatalf("参数分片拼接必须逐字节相等:\n want=%s\n got =%s", wantArgs, args)
	}
	var decoded struct {
		Path     string `json:"path"`
		MaxBytes int    `json:"max_bytes"`
	}
	if uErr := json.Unmarshal([]byte(args), &decoded); uErr != nil {
		t.Fatalf("拼接结果不是合法 JSON: %v（%q）", uErr, args)
	}
	if decoded.Path != "deep/dir/file.txt" || decoded.MaxBytes != 4096 {
		t.Fatalf("参数语义错误: %+v", decoded)
	}
}

// mustJSONString 把字符串编码成 JSON 字符串字面量（含引号），用于拼 SSE 帧。
func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestContractUsageOnlyFinalChunk
// 不变量：usage 只出现在收尾 chunk（choices 为空数组），
//
//	数值必须逐字段保真，且 Estimated=false（真实值不标估算）。
func TestContractUsageOnlyFinalChunk(t *testing.T) {
	srv, _ := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1234,"completion_tokens":56,"total_tokens":1290,"prompt_tokens_details":{"cached_tokens":1024}}}`,
	})
	p := newTestProvider(t, srv.URL, 5*time.Second)

	stream, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("ChatStream 失败: %v", err)
	}
	defer stream.Close()

	var usageChunks int
	var got *Usage
	var finish string
	for {
		ch, rErr := stream.Recv()
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			t.Fatalf("Recv 失败: %v", rErr)
		}
		if ch.Usage != nil {
			usageChunks++
			got = ch.Usage
		}
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
	}
	if finish != "stop" {
		t.Fatalf("finish_reason 应为 stop，得到 %q", finish)
	}
	if usageChunks != 1 {
		t.Fatalf("usage 应只出现 1 次，实际 %d 次", usageChunks)
	}
	if got == nil {
		t.Fatal("未解析到 usage")
	}
	if got.PromptTokens != 1234 || got.CompletionTokens != 56 || got.TotalTokens != 1290 || got.CachedTokens != 1024 {
		t.Fatalf("usage 数值未保真: %+v", got)
	}
	if got.Estimated {
		t.Fatal("上游真实 usage 不得标记为 Estimated")
	}
}

// TestContractUsageAbsentIsNotFabricated
// 不变量：上游未返回 usage 时，不得凭空伪造 Usage（上层据 nil 决定走估算路径）。
func TestContractUsageAbsentIsNotFabricated(t *testing.T) {
	srv, _ := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"content":"a"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	})
	p := newTestProvider(t, srv.URL, 5*time.Second)

	stream, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("ChatStream 失败: %v", err)
	}
	defer stream.Close()

	_, _, _, _, usage, n := drainStream(t, stream)
	if usage != nil {
		t.Fatalf("上游无 usage 时必须保持 nil，不得伪造，得到 %+v", usage)
	}
	if n == 0 {
		t.Fatal("正文增量未被消费，测试无效")
	}
}

// TestContractReasoningDeltaSeparatedFromContent
// 不变量：reasoning_content 走 Reasoning 字段，**绝不混入**正文 Delta
//
//	（否则思考过程会污染最终消息与落库正文）。
func TestContractReasoningDeltaSeparatedFromContent(t *testing.T) {
	srv, _ := sseServer(t, []string{
		`{"choices":[{"index":0,"delta":{"reasoning_content":"先想"}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"再看"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"答案"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	})
	p := newTestProvider(t, srv.URL, 5*time.Second)

	stream, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("ChatStream 失败: %v", err)
	}
	defer stream.Close()

	var content, reasoning string
	for {
		ch, rErr := stream.Recv()
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			t.Fatalf("Recv 失败: %v", rErr)
		}
		content += ch.Delta
		reasoning += ch.Reasoning
	}
	if content != "答案" {
		t.Fatalf("正文只应含 content，得到 %q", content)
	}
	if reasoning != "先想再看" {
		t.Fatalf("思考增量拼接错误，得到 %q", reasoning)
	}
}

// TestContractBuildBodyToolMessageRoundTrip
// 不变量：多轮工具往返时，assistant 的 tool_calls（含 arguments 原文）与
//
//	tool 消息的 tool_call_id 必须原样出现在请求体，供上游对齐调用链。
func TestContractBuildBodyToolMessageRoundTrip(t *testing.T) {
	var captured map[string]any
	srv := rawSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	p := newTestProvider(t, srv.URL, 5*time.Second)

	stream, err := p.ChatStream(context.Background(), ChatRequest{
		Model:  "m",
		Stream: true,
		Messages: []Message{
			{Role: RoleUser, Content: "读一下 a.txt"},
			{
				Role:    RoleAssistant,
				Content: "",
				ToolCalls: []ToolCall{{
					ID:        "call_1",
					Name:      "fs.read",
					Arguments: json.RawMessage(`{"path":"a.txt"}`),
				}},
			},
			{Role: RoleTool, Content: "文件内容", Name: "fs.read", ToolCallID: "call_1"},
		},
	})
	if err != nil {
		t.Fatalf("ChatStream 失败: %v", err)
	}
	defer stream.Close()

	msgs, ok := captured["messages"].([]any)
	if !ok || len(msgs) != 3 {
		t.Fatalf("应有 3 条消息，得到 %v", captured["messages"])
	}
	assistant, _ := msgs[1].(map[string]any)
	tcs, ok := assistant["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("assistant 消息应携带 1 个 tool_calls: %v", assistant)
	}
	tc, _ := tcs[0].(map[string]any)
	if tc["id"] != "call_1" || tc["type"] != "function" {
		t.Fatalf("tool_call 标识错误: %v", tc)
	}
	fn, _ := tc["function"].(map[string]any)
	if fn["name"] != "fs.read" || fn["arguments"] != `{"path":"a.txt"}` {
		t.Fatalf("tool_call function 透传错误（arguments 必须是原文 JSON 串）: %v", fn)
	}
	toolMsg, _ := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" {
		t.Fatalf("tool 消息必须带 tool_call_id: %v", toolMsg)
	}
	// 请求级不变量：流式必须显式要 usage
	so, _ := captured["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Fatalf("流式请求必须带 stream_options.include_usage=true: %v", captured["stream_options"])
	}
}

// TestContractNonRetryableStatusNotRetriedOnStreamOpen
// 不变量：400（请求被上游拒绝）不可重试，且错误分类为 bad_request，
//
//	保证「参数错误」不会被重试放大成重复计费。
func TestContractNonRetryableStatusNotRetriedOnStreamOpen(t *testing.T) {
	var calls int32
	srv := rawSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"max_tokens too large"}}`)
	})
	p := newTestProvider(t, srv.URL, 5*time.Second)

	_, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Stream: true})
	if err == nil {
		t.Fatal("400 必须返回错误")
	}
	le := AsError(err)
	if le.Kind != ErrKindBadRequest {
		t.Fatalf("400 应归类为 bad_request，得到 %s", le.Kind)
	}
	if le.Retryable() {
		t.Fatal("400 不可重试")
	}
	if le.StatusCode != http.StatusBadRequest {
		t.Fatalf("StatusCode 应为 400，得到 %d", le.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("不可重试错误应只请求 1 次，实际 %d", got)
	}
}
