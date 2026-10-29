package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// newTestServer 组装完整链路（store → agent → service → api）用于端到端测试。
func newTestServer(t *testing.T) (*httptest.Server, string) {
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

	// fake provider 无延迟：Run 会瞬间完成，正好验证「订阅晚到也能补发历史」
	provider := llm.NewFakeProvider()
	provider.Delay = 0

	engine, err := agent.New(agent.Deps{
		Sessions: service.NewSessionProvider(sessionStore),
		Summary:  service.NewSummaryWriter(mainStore),
		Events:   bus,
		Models:   &testResolver{provider: provider},
		Builder: prompt.NewBuilder(tokenizer.NewHeuristic(), prompt.Limits{
			MaxDeferredIndex: 60,
			MaxSOP:           6,
		}),
		Gate:     gate,
		MaxSteps: 5,
	})
	if err != nil {
		t.Fatalf("创建引擎失败: %v", err)
	}

	tokenSvc := auth.NewService(auth.Options{Main: mainStore})
	user, _, err := tokenSvc.EnsureBootstrapUser(ctx, "admin", "test-password-123")
	if err != nil {
		t.Fatalf("创建初始账号失败: %v", err)
	}
	_ = user
	plain, _, err := tokenSvc.EnsureBootstrapToken(ctx)
	if err != nil {
		t.Fatalf("生成初始令牌失败: %v", err)
	}

	sessionSvc := service.NewSessionService(mainStore, sessionStore)
	runSvc := service.NewRunService(service.RunServiceOptions{
		Main:       mainStore,
		Sessions:   sessionStore,
		Engine:     engine,
		SystemBase: "你是测试助手",
	})
	maintenanceSvc := service.NewMaintenanceService(mainStore, sessionStore, runSvc, gate.PendingCount)

	logger := observ.NewLogger("error", "text", io.Discard)
	router := NewRouter(Deps{
		Version:     "test",
		Log:         logger,
		Sessions:    sessionSvc,
		Runs:        runSvc,
		Maintenance: maintenanceSvc,
		Bus:         bus,
		Auth:        tokenSvc,
		AuthConfig: AuthConfig{
			SSOEnabled:   true,
			SSOMode:      "plain",
			SessionTTL:   time.Hour,
			SecureCookie: false,
			SSOLimiter:   auth.NewLimiter(5, time.Minute),
		},
		VerifyToken: tokenSvc.VerifyToken,
	})

	srv := httptest.NewServer(router)
	t.Cleanup(func() {
		srv.Close()
		bus.Close()
		_ = sessionStore.Close()
		_ = mainStore.Close()
	})
	return srv, plain
}

type testResolver struct{ provider llm.Provider }

func (r *testResolver) Resolve(context.Context, string) (llm.Provider, string, error) {
	return r.provider, "fake-model", nil
}

func doJSON(t *testing.T, method, url, token string, body any, wantStatus int, out any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s 请求失败: %v", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s 期望 %d，得到 %d: %s", method, url, wantStatus, resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("解析响应失败: %v", err)
		}
	}
}

// 全链路：healthz → 鉴权 → 建会话 → 起 Run → SSE 收流 → 消息落库 → 统计。
func TestEndToEndChatFlow(t *testing.T) {
	srv, token := newTestServer(t)

	// ① healthz 免鉴权
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var health map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&health)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || health["status"] != "ok" {
		t.Fatalf("healthz 异常: %d %v", resp.StatusCode, health)
	}
	if health["protocol_version"] != observ.ProtocolVersion {
		t.Fatalf("healthz 应暴露 protocol_version: %v", health)
	}

	// ② 无令牌 → 401
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions", "", nil, http.StatusUnauthorized, nil)
	// ③ 错误令牌 → 401（不区分原因）
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions", "hgt_wrong", nil, http.StatusUnauthorized, nil)

	// ④ 建会话
	var sess struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		ModelRef string `json:"model_ref"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions", token,
		map[string]any{"title": "端到端测试", "model_ref": "fake-model"}, http.StatusCreated, &sess)
	if sess.ID == "" {
		t.Fatal("会话 ID 为空")
	}

	// ⑤ 起 Run → 202
	var runResp struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions/"+sess.ID+"/runs", token,
		map[string]any{"input": "你好"}, http.StatusAccepted, &runResp)
	if runResp.RunID == "" {
		t.Fatal("run_id 为空")
	}

	// ⑥ SSE 收事件（fake provider 无延迟，Run 可能已完成 —— 验证历史补发）
	sseEvents := collectSSE(t, srv.URL, token, sess.ID, runResp.RunID)
	if len(sseEvents) == 0 {
		t.Fatal("未收到任何 SSE 事件")
	}
	var types []string
	var deltas strings.Builder
	for _, ev := range sseEvents {
		types = append(types, ev.Type)
		if ev.Type == string(events.TypeMessageDelta) {
			var p struct {
				Delta string `json:"delta"`
			}
			_ = json.Unmarshal([]byte(ev.Data), &p)
			deltas.WriteString(p.Delta)
		}
	}
	joined := strings.Join(types, ",")
	for _, want := range []string{
		string(events.TypeMessageStart),
		string(events.TypeMessageDelta),
		string(events.TypeMessageEnd),
		string(events.TypeRunStatus),
		string(events.TypeResync),
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("事件流缺少 %s，实际: %s", want, joined)
		}
	}
	if !strings.Contains(deltas.String(), "收到：你好") {
		t.Fatalf("delta 拼接结果异常: %q", deltas.String())
	}

	// ⑦ Run 终态
	var run struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/"+sess.ID+"/runs/"+runResp.RunID, token,
		nil, http.StatusOK, &run)
	if run.Status != "completed" {
		t.Fatalf("Run 应为 completed，得到 %s", run.Status)
	}

	// ⑧ 消息落库（DB 才是权威）：user + assistant
	var msgs struct {
		Items []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			Seq     int64  `json:"seq"`
		} `json:"items"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/"+sess.ID+"/messages", token,
		nil, http.StatusOK, &msgs)
	if len(msgs.Items) != 2 {
		t.Fatalf("应落 2 条消息（user + assistant），得到 %d", len(msgs.Items))
	}
	if msgs.Items[0].Role != "user" || msgs.Items[0].Content != "你好" {
		t.Fatalf("第 1 条应为用户消息: %+v", msgs.Items[0])
	}
	if msgs.Items[1].Role != "assistant" || !strings.Contains(msgs.Items[1].Content, "收到：你好") {
		t.Fatalf("第 2 条应为助手回复: %+v", msgs.Items[1])
	}
	if msgs.Items[1].Seq != 2 {
		t.Fatalf("seq 应递增为 2，得到 %d", msgs.Items[1].Seq)
	}

	// ⑨ 会话列表只读主库，摘要列已回写
	var list struct {
		Items []struct {
			ID           string `json:"id"`
			MessageCount int64  `json:"message_count"`
			LastRunState string `json:"last_run_status"`
		} `json:"items"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions", token, nil, http.StatusOK, &list)
	if len(list.Items) != 1 {
		t.Fatalf("应列出 1 个会话，得到 %d", len(list.Items))
	}
	if list.Items[0].MessageCount != 2 {
		t.Fatalf("摘要 message_count 应为 2，得到 %d", list.Items[0].MessageCount)
	}
	if list.Items[0].LastRunState != "completed" {
		t.Fatalf("摘要 last_run_status 应为 completed，得到 %q", list.Items[0].LastRunState)
	}

	// ⑩ 维护统计（双库视图）
	var stats struct {
		ProtocolVersion string `json:"protocol_version"`
		Sessions        int64  `json:"sessions"`
		OpenSessionDBs  int    `json:"open_session_dbs"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/settings/maintenance", token, nil, http.StatusOK, &stats)
	if stats.Sessions != 1 || stats.OpenSessionDBs < 1 {
		t.Fatalf("统计异常: %+v", stats)
	}

	// ⑪ 删除会话 → 204，再取 → 404
	doJSON(t, http.MethodDelete, srv.URL+"/api/v1/sessions/"+sess.ID, token, nil, http.StatusNoContent, nil)
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/"+sess.ID, token, nil, http.StatusNotFound, nil)
}

// 请求体字段漂移必须被拒绝（DisallowUnknownFields）。
func TestUnknownFieldRejected(t *testing.T) {
	srv, token := newTestServer(t)
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions", token,
		map[string]any{"title": "x", "unknown_field": 1}, http.StatusBadRequest, nil)
}

// 不存在的会话 → 404。
func TestSessionNotFound(t *testing.T) {
	srv, token := newTestServer(t)
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/nope", token, nil, http.StatusNotFound, nil)
}

// 空输入 → 400。
func TestCreateRunRequiresInput(t *testing.T) {
	srv, token := newTestServer(t)
	var sess struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions", token,
		map[string]any{"title": "t", "model_ref": "m"}, http.StatusCreated, &sess)
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions/"+sess.ID+"/runs", token,
		map[string]any{"input": ""}, http.StatusBadRequest, nil)
}

// 限流：连续超过 burst 后返回 429。
func TestRateLimit(t *testing.T) {
	srv, token := newTestServer(t)
	limited := false
	for i := 0; i < 60; i++ {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/sessions", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("连续请求应触发限流 429")
	}
}

// ============ SSE 辅助 ============

type sseEvent struct {
	Seq  int64
	Type string
	Data string
}

// collectSSE 读取 SSE 直到 resync 事件或流关闭。
func collectSSE(t *testing.T, baseURL, token, sid, rid string) []sseEvent {
	t.Helper()
	url := baseURL + "/api/v1/sessions/" + sid + "/runs/" + rid + "/events"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("SSE 连接失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("SSE 状态码 %d: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type 应为 text/event-stream，得到 %s", ct)
	}

	var out []sseEvent
	var cur sseEvent
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			if n, err := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64); err == nil {
				cur.Seq = n
			}
		case strings.HasPrefix(line, "event: "):
			cur.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if cur.Type != "" {
				out = append(out, cur)
				if cur.Type == string(events.TypeResync) {
					return out
				}
				cur = sseEvent{}
			}
		}
	}
	return out
}
