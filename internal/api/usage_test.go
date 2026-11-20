package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"harness-go/internal/auth"
	"harness-go/internal/model"
	"harness-go/internal/observ"
	"harness-go/internal/service"
	"harness-go/internal/store"
)

// 会话用量汇总端点（P1-2）：GET /api/v1/sessions/{sid}/usage
//
// 口径铁律：runs.input_tokens 是**逐步累加账单**（每轮 AddUsage `+=`），不是单次
// 请求量。端点必须返回 SUM 真值，前端命中率按 cached/tokens_in 计算。
//
// 本组测试锁死两条契约：
//   ① 有 Run 时返回值等于 runs 表 SUM 真值（含已完成 / 已取消等所有终态）；
//   ② 空会话返回全 0 + **200**（不是 404）——「还没用」不是错误，
//      否则前端刷新后用量面板会报错而非显示 0。

// usageEnv 用量端点测试环境（只装配认证 + 会话域 + Run 域，不接引擎）。
type usageEnv struct {
	srv      *httptest.Server
	token    string
	main     *store.MainStore
	sessions *store.SessionStore
	ctx      context.Context
}

func newUsageEnv(t *testing.T) *usageEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	main, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	sessions, err := store.OpenSessionStore(store.SessionStoreOptions{DataDir: dir, MaxOpen: 4})
	if err != nil {
		t.Fatalf("打开会话库失败: %v", err)
	}
	t.Cleanup(func() { _ = sessions.Close() })

	authSvc := auth.NewService(auth.Options{Main: main})
	if _, _, err := authSvc.EnsureBootstrapUser(ctx, "admin", "test-password-123"); err != nil {
		t.Fatalf("创建 owner 账号失败: %v", err)
	}
	plain, _, err := authSvc.EnsureBootstrapToken(ctx)
	if err != nil {
		t.Fatalf("签发令牌失败: %v", err)
	}

	router := NewRouter(Deps{
		Version:     "test",
		Log:         observ.NewLogger("error", "text", io.Discard),
		Sessions:    service.NewSessionService(main, sessions),
		Runs:        service.NewRunService(service.RunServiceOptions{Main: main, Sessions: sessions}),
		Projects:    service.NewProjectService(main),
		Workspaces:  service.NewWorkspaceService(main, nil),
		Auth:        authSvc,
		VerifyToken: authSvc.VerifyToken,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return &usageEnv{srv: srv, token: plain, main: main, sessions: sessions, ctx: ctx}
}

// createSession 建一条会话并返回 ID。
func (e *usageEnv) createSession(t *testing.T, title string) string {
	t.Helper()
	var out struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, e.srv.URL+"/api/v1/sessions", e.token,
		map[string]any{"title": title, "model_ref": "fake-model"}, http.StatusCreated, &out)
	return out.ID
}

// usageResp 用量端点响应。
type usageResp struct {
	TokensIn     int64 `json:"tokens_in"`
	TokensOut    int64 `json:"tokens_out"`
	CachedTokens int64 `json:"cached_tokens"`
	Runs         int64 `json:"runs"`
}

// seedRun 直接往会话库造一条 Run 并累加用量（绕开引擎，专注聚合口径）。
func (e *usageEnv) seedRun(t *testing.T, sessionID, runID string,
	status model.RunStatus, in, out, cached int64) {
	t.Helper()
	sdb, err := e.sessions.Acquire(e.ctx, sessionID)
	if err != nil {
		t.Fatalf("打开会话库失败: %v", err)
	}
	if err := sdb.Runs().Create(e.ctx, &model.Run{
		ID: runID, Status: status, ModelRef: "fake-model",
	}); err != nil {
		t.Fatalf("创建 Run %s 失败: %v", runID, err)
	}
	if in != 0 || out != 0 || cached != 0 {
		if err := sdb.Runs().AddUsage(e.ctx, runID, in, out, cached, true); err != nil {
			t.Fatalf("回写 Run %s 用量失败: %v", runID, err)
		}
	}
}

// TestSessionUsageSummary 有 Run（含已完成/已取消）时端点等于 runs 表 SUM 真值。
func TestSessionUsageSummary(t *testing.T) {
	env := newUsageEnv(t)
	sid := env.createSession(t, "用量汇总")

	env.seedRun(t, sid, "run-1", model.RunCompleted, 1000, 200, 800)
	env.seedRun(t, sid, "run-2", model.RunCancelled, 500, 50, 400)
	env.seedRun(t, sid, "run-3", model.RunFailed, 0, 0, 0)

	// 期望值直接对 SQL 求和取真值（不硬编码，避免与实现共享同一个错口径）。
	sdb, err := env.sessions.Acquire(env.ctx, sid)
	if err != nil {
		t.Fatalf("打开会话库失败: %v", err)
	}
	wantIn, wantOut, wantCached, wantRuns, err := sdb.Runs().UsageSummary(env.ctx)
	if err != nil {
		t.Fatalf("取真值失败: %v", err)
	}
	if wantRuns != 3 {
		t.Fatalf("应造出 3 条 Run，实际 %d", wantRuns)
	}

	var got usageResp
	doJSON(t, http.MethodGet, env.srv.URL+"/api/v1/sessions/"+sid+"/usage", env.token,
		nil, http.StatusOK, &got)
	if got.TokensIn != wantIn || got.TokensOut != wantOut ||
		got.CachedTokens != wantCached || got.Runs != wantRuns {
		t.Fatalf("端点返回值与 runs 表 SUM 不一致：got=%+v want={in:%d out:%d cached:%d runs:%d}",
			got, wantIn, wantOut, wantCached, wantRuns)
	}
	// 独立断言具体数字（防「真值也取错」时测试自洽通过）。
	const expIn, expOut, expCached = 1500, 250, 1200
	if got.TokensIn != expIn || got.TokensOut != expOut || got.CachedTokens != expCached {
		t.Fatalf("用量聚合错误：got=%+v want={in:%d out:%d cached:%d}", got, expIn, expOut, expCached)
	}
}

// TestSessionUsageSummaryEmpty 空会话 / 不存在的会话 → 全 0 + 200（不是 404）。
func TestSessionUsageSummaryEmpty(t *testing.T) {
	env := newUsageEnv(t)
	sid := env.createSession(t, "空会话")

	var got usageResp
	doJSON(t, http.MethodGet, env.srv.URL+"/api/v1/sessions/"+sid+"/usage", env.token,
		nil, http.StatusOK, &got)
	if got.TokensIn != 0 || got.TokensOut != 0 || got.CachedTokens != 0 || got.Runs != 0 {
		t.Fatalf("空会话应返回全 0，得到 %+v", got)
	}

	// 完全没跑过 Run 的会话 id（会话库惰性创建）：仍是 200 + 全 0。
	var raw map[string]any
	doJSON(t, http.MethodGet, env.srv.URL+"/api/v1/sessions/never-used/usage", env.token,
		nil, http.StatusOK, &raw)
	rawJSON, _ := json.Marshal(raw)
	if string(rawJSON) != `{"cached_tokens":0,"runs":0,"tokens_in":0,"tokens_out":0}` {
		t.Fatalf("空会话响应体形状不符：%s", rawJSON)
	}
}
