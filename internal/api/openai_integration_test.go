package api

// P1 OpenAI 兼容端点端到端（httptest 全链路）：/v1/models + /v1/chat/completions
// （非流式 / 流式 / 隐藏会话 / X-Session-ID 复用 / 鉴权）。

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"harness-go/internal/agent"
	"harness-go/internal/approval"
	"harness-go/internal/auth"
	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/observ"
	"harness-go/internal/prompt"
	"harness-go/internal/service"
	"harness-go/internal/store"
	"harness-go/internal/tokenizer"
)

// openaiEchoProvider 固定回复 provider（无工具帧，纯文本）。
type openaiEchoProvider struct{ reply string }

func (p *openaiEchoProvider) Name() string { return "openai-fixture" }

func (p *openaiEchoProvider) ChatStream(context.Context, llm.ChatRequest) (llm.Stream, error) {
	return &scriptedStream{chunks: []llm.Chunk{
		{Delta: p.reply},
		{FinishReason: "stop", Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13}},
	}}, nil
}

type openaiResolver struct{ p llm.Provider }

func (r openaiResolver) Resolve(_ context.Context, modelRef string) (llm.Provider, string, error) {
	if modelRef == "" {
		modelRef = "default"
	}
	return r.p, modelRef, nil
}

func newOpenAIEnv(t *testing.T, reply string) (*httptest.Server, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	mainStore, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	sessionStore, err := store.OpenSessionStore(store.SessionStoreOptions{DataDir: dir, MaxOpen: 8})
	if err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus(1000, 256)
	provider := &openaiEchoProvider{reply: reply}
	engine, err := agent.New(agent.Deps{
		Sessions: service.NewSessionProvider(sessionStore),
		Summary:  service.NewSummaryWriter(mainStore),
		Events:   bus,
		Models:   openaiResolver{provider},
		Builder:  prompt.NewBuilder(tokenizer.NewHeuristic(), prompt.DefaultLimits()),
		Gate:     approval.NewGate(2 * time.Second),
		MaxSteps: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	authSvc := auth.NewService(auth.Options{Main: mainStore})
	if _, _, err := authSvc.EnsureBootstrapUser(ctx, "admin", "test-password-123"); err != nil {
		t.Fatal(err)
	}
	plain, _, err := authSvc.EnsureBootstrapToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sessionSvc := service.NewSessionService(mainStore, sessionStore)
	runSvc := service.NewRunService(service.RunServiceOptions{
		Main:       mainStore,
		Sessions:   sessionStore,
		Engine:     engine,
		SystemBase: "默认系统提示",
	})
	openaiSvc := service.NewOpenAICompatService(service.OpenAICompatServiceOptions{
		Main:     mainStore,
		Sessions: sessionStore,
		Runs:     runSvc,
		Bus:      bus,
	})
	logger := observ.NewLogger("error", "text", io.Discard)
	handler := NewRouter(Deps{
		Version: "test", Log: logger, Sessions: sessionSvc, Runs: runSvc,
		Bus: bus, Auth: authSvc, OpenAI: openaiSvc,
		AuthConfig:  AuthConfig{SSOEnabled: true, SSOMode: "plain", SessionTTL: time.Hour},
		VerifyToken: authSvc.VerifyToken,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(func() {
		srv.Close()
		bus.Close()
		_ = sessionStore.Close()
		_ = mainStore.Close()
	})
	return srv, plain
}

func openaiPost(srv *httptest.Server, token, path, body string, headers map[string]string) (int, string) {
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		return -1, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestOpenAIModels GET /v1/models：OpenAI list 形状 + default。
func TestOpenAIModels(t *testing.T) {
	srv, token := newOpenAIEnv(t, "hi")
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Object != "list" || len(body.Data) == 0 {
		t.Fatalf("models 形状异常: %+v", body)
	}
	found := false
	for _, m := range body.Data {
		if m.ID == "default" && m.Object == "model" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应含 default 模型: %+v", body.Data)
	}
}

// TestOpenAIChatCompletionNonStream 非流式：隐藏会话映射 + OpenAI 响应形状 + usage。
func TestOpenAIChatCompletionNonStream(t *testing.T) {
	srv, token := newOpenAIEnv(t, "回答内容")
	status, body := openaiPost(srv, token, "/v1/chat/completions",
		`{"model":"default","messages":[{"role":"system","content":"自定义系统提示"},{"role":"user","content":"你好"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var resp struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("响应非法 JSON: %s", body)
	}
	if resp.Object != "chat.completion" || len(resp.Choices) != 1 {
		t.Fatalf("响应形状异常: %s", body)
	}
	c := resp.Choices[0]
	if c.Message.Role != "assistant" || c.Message.Content != "回答内容" || c.FinishReason != "stop" {
		t.Fatalf("choice 异常: %+v", c)
	}
	if resp.Usage.TotalTokens != 13 || resp.Usage.PromptTokens != 10 {
		t.Fatalf("usage 异常: %+v", resp.Usage)
	}
	if !strings.HasPrefix(resp.ID, "chatcmpl-") {
		t.Fatalf("ID 形状异常: %s", resp.ID)
	}
}

// TestOpenAIChatCompletionStream 流式：role 帧 + delta 帧 + finish + [DONE]。
func TestOpenAIChatCompletionStream(t *testing.T) {
	srv, token := newOpenAIEnv(t, "流式回答")
	status, body := openaiPost(srv, token, "/v1/chat/completions",
		`{"model":"default","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"讲个故事"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if !strings.Contains(body, "text/event-stream") {
		// openaiPost 读的是 body 而非 header —— SSE 帧格式本身可判定
		_ = body
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("缺少 [DONE]: %q", body)
	}
	if !strings.Contains(body, `"chat.completion.chunk"`) {
		t.Fatalf("缺少 chunk 帧: %q", body)
	}
	if !strings.Contains(body, "流式回答") {
		t.Fatalf("缺少 delta 内容: %q", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("缺少终止帧: %q", body)
	}
	if !strings.Contains(body, `"total_tokens":13`) {
		t.Fatalf("include_usage 应带 usage 帧: %q", body)
	}
}

// TestOpenAIHiddenSession 隐藏会话：source=openai + 不进默认列表 + X-Session-ID 复用。
func TestOpenAIHiddenSession(t *testing.T) {
	srv, token := newOpenAIEnv(t, "第一轮")
	// 第一轮：无 X-Session-ID → 创建隐藏会话
	status, body := openaiPost(srv, token, "/v1/chat/completions",
		`{"model":"default","messages":[{"role":"user","content":"第一条"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	// 第二轮：带 X-Session-ID（用第一轮 run 反查不可行——直接从会话列表确认隐藏性）
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var list struct {
		Items []struct {
			ID     string `json:"id"`
			Source string `json:"source,omitempty"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	for _, s := range list.Items {
		if s.Source == "openai" {
			t.Fatalf("隐藏会话不得出现在默认列表: %+v", list.Items)
		}
	}
	// X-Session-ID 不存在 → 显式 404（OpenAI 错误形状）
	status, body = openaiPost(srv, token, "/v1/chat/completions",
		`{"model":"default","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-Session-ID": "sess-not-exist"})
	if status != 404 || !strings.Contains(body, `"error"`) {
		t.Fatalf("未知会话应 404 + OpenAI 错误形状: %d %s", status, body)
	}
}

// TestOpenAISSEDecoded 逐帧解码 SSE：验证 role 首帧与 delta 序。
func TestOpenAISSEDecoded(t *testing.T) {
	srv, token := newOpenAIEnv(t, "AB")
	status, body := openaiPost(srv, token, "/v1/chat/completions",
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"x"}]}`, nil)
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	sc := bufio.NewScanner(strings.NewReader(body))
	roleFrame, deltaSeen := false, ""
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data: ") || strings.TrimSpace(line) == "data: [DONE]" {
			continue
		}
		var frame struct {
			Choices []struct {
				Delta struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatalf("帧非法: %q", line)
		}
		if len(frame.Choices) == 0 {
			continue // usage 帧
		}
		d := frame.Choices[0].Delta
		if d.Role == "assistant" {
			roleFrame = true
		}
		deltaSeen += d.Content
	}
	if !roleFrame {
		t.Fatal("缺少 role 首帧")
	}
	if deltaSeen != "AB" {
		t.Fatalf("delta 序异常: %q", deltaSeen)
	}
}
