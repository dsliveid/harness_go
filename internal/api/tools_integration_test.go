package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"harness-go/internal/agent"
	"harness-go/internal/approval"
	"harness-go/internal/auth"
	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/observ"
	"harness-go/internal/plugins/echo"
	"harness-go/internal/prompt"
	"harness-go/internal/service"
	"harness-go/internal/store"
	"harness-go/internal/tokenizer"
	"harness-go/internal/tools"
	"harness-go/internal/tools/hostimpl"
	"harness-go/internal/tools/loader"
	"harness-go/internal/tools/registry"
	toolrouter "harness-go/internal/tools/router"
	"harness-go/pkg/toolapi"
)

// scriptedProvider 第一轮发起 echo 工具调用，第二轮给出最终回答（工具链路 fixture）。
type scriptedProvider struct {
	mu    sync.Mutex
	calls int
}

// Name 实现 llm.Provider。
func (p *scriptedProvider) Name() string { return "scripted" }

// ChatStream 实现 llm.Provider。
func (p *scriptedProvider) ChatStream(context.Context, llm.ChatRequest) (llm.Stream, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()

	if n == 1 {
		return &scriptedStream{chunks: []llm.Chunk{
			{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "echo", ArgumentsDelta: `{"text":"hello"}`}},
			{FinishReason: "tool_calls"},
		}}, nil
	}
	return &scriptedStream{chunks: []llm.Chunk{
		{Delta: "已收到工具结果"},
		{FinishReason: "stop", Usage: &llm.Usage{PromptTokens: 20, CompletionTokens: 6, TotalTokens: 26}},
	}}, nil
}

type scriptedStream struct {
	chunks []llm.Chunk
	i      int
}

func (s *scriptedStream) Recv() (llm.Chunk, error) {
	if s.i >= len(s.chunks) {
		return llm.Chunk{}, llm.EOF
	}
	c := s.chunks[s.i]
	s.i++
	return c, nil
}

func (s *scriptedStream) Close() error { return nil }

// newToolsTestServer 组装带工具子系统的完整链路（store → tools → agent → service → api）。
func newToolsTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	mainStore, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	sessionStore, err := store.OpenSessionStore(store.SessionStoreOptions{DataDir: dir, MaxOpen: 8})
	if err != nil {
		t.Fatalf("打开会话库失败: %v", err)
	}
	bus := events.NewBus(1000, 256)
	gate := approval.NewGate(2 * time.Second)

	// 工具子系统：注册表 + 执行面路由 + 执行适配 + Host
	toolReg := registry.New(registry.Options{CoreSoftLimit: 8, DeferredSoftLimit: 60})
	toolHost := hostimpl.New(hostimpl.Options{WorkDir: dir})
	if err := toolReg.RegisterFactory(ctx, echo.Factory(), toolHost, nil); err != nil {
		t.Fatalf("echo fixture 装配失败: %v", err)
	}
	toolRunner := tools.NewRunner(toolReg, toolrouter.New(toolrouter.Options{
		Executor: toolapi.ExecutorServer,
		Capabilities: map[string]bool{
			toolapi.CapWorkspaceRead:  true,
			toolapi.CapWorkspaceWrite: true,
			toolapi.CapProcess:        true,
			toolapi.CapNetwork:        true,
			toolapi.CapLLM:            true,
		},
	}), loader.Limits{CoreSoft: 8, DeferredSoft: 60})

	engine, err := agent.New(agent.Deps{
		Sessions: service.NewSessionProvider(sessionStore),
		Summary:  service.NewSummaryWriter(mainStore),
		Events:   bus,
		Models:   &testResolver{provider: &scriptedProvider{}},
		Builder: prompt.NewBuilder(tokenizer.NewHeuristic(), prompt.Limits{
			MaxDeferredIndex: 60,
			MaxSOP:           6,
		}),
		Tools:    toolRunner,
		Catalog:  toolRunner,
		Gate:     gate,
		MaxSteps: 5,
	})
	if err != nil {
		t.Fatalf("创建引擎失败: %v", err)
	}

	authSvc := auth.NewService(auth.Options{Main: mainStore})
	if _, _, err := authSvc.EnsureBootstrapUser(ctx, "admin", "test-password-123"); err != nil {
		t.Fatalf("创建初始账号失败: %v", err)
	}
	plain, _, err := authSvc.EnsureBootstrapToken(ctx)
	if err != nil {
		t.Fatalf("生成初始令牌失败: %v", err)
	}

	sessionSvc := service.NewSessionService(mainStore, sessionStore)
	runSvc := service.NewRunService(service.RunServiceOptions{
		Main:       mainStore,
		Sessions:   sessionStore,
		Engine:     engine,
		SystemBase: "你是测试助手",
		ToolRefs:   toolRunner.PromptRefs,
	})
	maintenanceSvc := service.NewMaintenanceService(mainStore, sessionStore, runSvc, gate.PendingCount)
	toolSvc := service.NewToolService(toolRunner, mainStore)

	logger := observ.NewLogger("error", "text", io.Discard)
	handler := NewRouter(Deps{
		Version:     "test",
		Log:         logger,
		Sessions:    sessionSvc,
		Runs:        runSvc,
		Maintenance: maintenanceSvc,
		Tools:       toolSvc,
		Bus:         bus,
		Auth:        authSvc,
		AuthConfig: AuthConfig{
			SSOEnabled: true, SSOMode: "plain", SessionTTL: time.Hour,
			SSOLimiter: auth.NewLimiter(5, time.Minute),
		},
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

// TestToolsSnapshotEndpoint 校验 GET /api/v1/tools：tier / default_tier / cost / health。
func TestToolsSnapshotEndpoint(t *testing.T) {
	srv, token := newToolsTestServer(t)

	var snap struct {
		Total int `json:"total"`
		Core  []struct {
			Name        string `json:"name"`
			Tier        string `json:"tier"`
			DefaultTier string `json:"default_tier"`
			CostTokens  int    `json:"cost_tokens"`
			Risk        string `json:"risk"`
			Health      struct {
				Status string `json:"status"`
			} `json:"health"`
		} `json:"core"`
		Limits struct {
			CoreSoft     int `json:"core_soft"`
			DeferredSoft int `json:"deferred_soft"`
		} `json:"limits"`
		Plugins []struct {
			ID string `json:"id"`
		} `json:"plugins"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/tools", token, nil, http.StatusOK, &snap)

	if snap.Total != 1 || len(snap.Core) != 1 {
		t.Fatalf("应只有 1 个 core 工具（echo），得到 total=%d core=%d", snap.Total, len(snap.Core))
	}
	echo := snap.Core[0]
	if echo.Name != "echo" || echo.Tier != "core" || echo.DefaultTier != "core" {
		t.Fatalf("echo 档位信息异常: %+v", echo)
	}
	if echo.CostTokens <= 0 {
		t.Fatalf("成本必须可见，得到 %d", echo.CostTokens)
	}
	if echo.Risk != "readonly" {
		t.Fatalf("echo 风险级应为 readonly，得到 %q", echo.Risk)
	}
	if echo.Health.Status != "available" {
		t.Fatalf("健康态应为 available，得到 %q", echo.Health.Status)
	}
	if snap.Limits.CoreSoft != 8 || snap.Limits.DeferredSoft != 60 {
		t.Fatalf("软上限回显异常: %+v", snap.Limits)
	}
	if len(snap.Plugins) != 1 || snap.Plugins[0].ID != "builtin.echo" {
		t.Fatalf("插件清单异常: %+v", snap.Plugins)
	}
}

// TestToolsInjectionAndTierPatch 校验注入预算端点与 tier 改档（下一轮生效）。
func TestToolsInjectionAndTierPatch(t *testing.T) {
	srv, token := newToolsTestServer(t)

	var inj struct {
		CostCore  int `json:"cost_core"`
		CostTotal int `json:"cost_total"`
		Core      []struct {
			Name string `json:"name"`
		} `json:"core"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/tools/injection", token, nil, http.StatusOK, &inj)
	if len(inj.Core) != 1 || inj.CostCore <= 0 || inj.CostTotal != inj.CostCore {
		t.Fatalf("注入预算明细异常: %+v", inj)
	}

	// 改档为 on_demand：core 清空、on_demand 出现
	var patched struct {
		Core     []struct{} `json:"core"`
		OnDemand []struct {
			Name string `json:"name"`
		} `json:"on_demand"`
		Overrides map[string]string `json:"overrides"`
	}
	doJSON(t, http.MethodPatch, srv.URL+"/api/v1/tools/tiers", token,
		map[string]any{"tiers": map[string]string{"echo": "on_demand"}}, http.StatusOK, &patched)
	if len(patched.OnDemand) != 1 || patched.OnDemand[0].Name != "echo" {
		t.Fatalf("改档后应进入 on_demand: %+v", patched)
	}
	if patched.Overrides["echo"] != "on_demand" {
		t.Fatalf("覆盖应被记录: %+v", patched.Overrides)
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/tools/injection", token, nil, http.StatusOK, &inj)
	if len(inj.Core) != 0 {
		t.Fatalf("改档后 core 应为空，得到 %d", len(inj.Core))
	}

	// 非法档位 / 未知工具 → 400
	doJSON(t, http.MethodPatch, srv.URL+"/api/v1/tools/tiers", token,
		map[string]any{"tiers": map[string]string{"echo": "bogus"}}, http.StatusBadRequest, nil)
	doJSON(t, http.MethodPatch, srv.URL+"/api/v1/tools/tiers", token,
		map[string]any{"tiers": map[string]string{"ghost": "core"}}, http.StatusBadRequest, nil)
	doJSON(t, http.MethodPatch, srv.URL+"/api/v1/tools/tiers", token,
		map[string]any{"tiers": map[string]string{}}, http.StatusBadRequest, nil)
}

// TestEchoToolInvokedAndRecorded 出口验收：模型真实调用 echo 且 tool_events 落库。
func TestEchoToolInvokedAndRecorded(t *testing.T) {
	srv, token := newToolsTestServer(t)

	var sess struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions", token,
		map[string]any{"title": "工具测试", "model_ref": "fake-model"}, http.StatusCreated, &sess)

	var created struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions/"+sess.ID+"/runs", token,
		map[string]any{"input": "请回显 hello"}, http.StatusAccepted, &created)
	if created.Status != "pending" && created.Status != "running" {
		t.Fatalf("Run 初始状态异常: %q", created.Status)
	}

	// 等 Run 收敛（两轮模型调用：tool_calls → 最终回答）
	var run struct {
		Status       string `json:"status"`
		StopReason   string `json:"stop_reason"`
		ErrorMessage string `json:"error_message"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/"+sess.ID+"/runs/"+created.RunID, token, nil, http.StatusOK, &run)
		if run.Status == "completed" || run.Status == "failed" || run.Status == "cancelled" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if run.Status != "completed" {
		t.Fatalf("Run 未成功收敛: status=%q stop=%q err=%q", run.Status, run.StopReason, run.ErrorMessage)
	}

	// tool_events 必须有 echo 的成功记录（风险级取自工具契约而非硬编码）
	var evs struct {
		Items []struct {
			ToolName string `json:"tool_name"`
			Status   string `json:"status"`
			Risk     string `json:"risk"`
			Executor string `json:"executor"`
			Result   struct {
				Content string `json:"content"`
			} `json:"result"`
		} `json:"items"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/"+sess.ID+"/runs/"+created.RunID+"/tool-events",
		token, nil, http.StatusOK, &evs)
	if len(evs.Items) != 1 {
		t.Fatalf("应有 1 条 tool_event，得到 %d", len(evs.Items))
	}
	ev := evs.Items[0]
	if ev.ToolName != "echo" || ev.Status != "ok" {
		t.Fatalf("tool_event 异常: %+v", ev)
	}
	if ev.Risk != "readonly" || ev.Executor != "server" {
		t.Fatalf("风险级/执行面应来自工具契约: %+v", ev)
	}
	if !strings.Contains(ev.Result.Content, "echo: hello") {
		t.Fatalf("工具结果未落库: %q", ev.Result.Content)
	}

	// 消息序列必须含 tool 消息（工具结果以 DB 为准）
	var msgs struct {
		Items []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"items"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/"+sess.ID+"/messages", token, nil, http.StatusOK, &msgs)
	var hasToolMsg, hasAssistant bool
	for _, m := range msgs.Items {
		if m.Role == "tool" && strings.Contains(m.Content, "echo: hello") {
			hasToolMsg = true
		}
		if m.Role == "assistant" && strings.Contains(m.Content, "已收到工具结果") {
			hasAssistant = true
		}
	}
	if !hasToolMsg || !hasAssistant {
		t.Fatalf("消息序列不完整: tool=%v assistant=%v items=%+v", hasToolMsg, hasAssistant, msgs.Items)
	}
}
