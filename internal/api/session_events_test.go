package api

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

	"harness-go/internal/auth"
	"harness-go/internal/model"
	"harness-go/internal/observ"
	"harness-go/internal/service"
	"harness-go/internal/sessionbus"
	"harness-go/internal/store"
)

// newSessionEventsEnv 组装一个**带会话域 + 会话总线**的最小链路。
//
// 为什么不复用 newDataTestEnv：它没有装配 Deps.Sessions（会话域），
// 用它测本端点会先撞「会话域未启用」→ 假阴性（看起来通了其实没测到推送）。
// 这是本项目已记录过的脚手架坑，故这里自建 env。
func newSessionEventsEnv(t *testing.T) (*httptest.Server, *sessionbus.Bus, *store.MainStore, string) {
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

	bus := sessionbus.New(sessionbus.Options{MergeWindow: -1}) // 测试里不做合并，逐帧可断言
	router := NewRouter(Deps{
		Version:       "test",
		Log:           observ.NewLogger("error", "text", io.Discard),
		Sessions:      service.NewSessionService(main, sessions),
		SessionEvents: bus,
		// 领域列表事件测试需要项目/工作空间域（否则 503 会先挡住写入路径）。
		Projects:    service.NewProjectService(main),
		Workspaces:  service.NewWorkspaceService(main, nil),
		Auth:        authSvc,
		VerifyToken: authSvc.VerifyToken,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv, bus, main, plain
}

// readSessionFrames 读取 n 帧 SSE，返回 (event, data) 列表（跳过 : ping 注释行）。
func readSessionFrames(t *testing.T, r *bufio.Reader, n int) []struct{ Event, Data string } {
	t.Helper()
	out := make([]struct{ Event, Data string }, 0, n)
	var event string
	for len(out) < n {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("读取 SSE 帧失败：%v（已收到 %d 帧）", err, len(out))
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, ":"):
			continue // 保活注释
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			out = append(out, struct{ Event, Data string }{event, strings.TrimPrefix(line, "data: ")})
			event = ""
		}
	}
	return out
}

// openSessionSSE 建立会话列表 SSE 连接。
func openSessionSSE(t *testing.T, srv *httptest.Server, token string) (*bufio.Reader, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/sessions/events", nil)
	if err != nil {
		cancel()
		t.Fatalf("构造请求失败：%v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("建立 SSE 连接失败：%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("SSE 应返回 200，得到 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		cancel()
		t.Fatalf("Content-Type 应为 text/event-stream，得到 %q", ct)
	}
	t.Cleanup(cancel)
	return bufio.NewReader(resp.Body), cancel
}

// seedSession 直接在库里造一条会话（绕开 HTTP，测试关注的是推送而非创建）。
func seedSession(t *testing.T, main *store.MainStore, id, title string) *model.Session {
	t.Helper()
	now := time.Now().UnixMilli()
	s := &model.Session{
		ID: id, Title: title, AccessMode: "confirm",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := main.Sessions().Create(context.Background(), s); err != nil {
		t.Fatalf("造会话失败: %v", err)
	}
	return s
}

// 首帧必须是全量快照，且包含已存在的会话。
func TestSessionEventsSnapshotFrame(t *testing.T) {
	srv, _, main, token := newSessionEventsEnv(t)
	seedSession(t, main, "s-1", "已有会话")

	r, cancel := openSessionSSE(t, srv, token)
	defer cancel()

	frames := readSessionFrames(t, r, 1)
	if frames[0].Event != sessionbus.EventSnapshot {
		t.Fatalf("首帧 event 应为 snapshot，得到 %q", frames[0].Event)
	}
	var payload struct {
		Type  string          `json:"type"`
		Items []model.Session `json:"items"`
		At    int64           `json:"at"`
	}
	if err := json.Unmarshal([]byte(frames[0].Data), &payload); err != nil {
		t.Fatalf("快照帧解析失败: %v", err)
	}
	if len(payload.Items) != 1 || payload.Items[0].ID != "s-1" {
		t.Fatalf("快照应含 s-1，实际 %+v", payload.Items)
	}
	if payload.At == 0 {
		t.Fatal("快照帧应带 at 时间戳")
	}
}

// 新建会话后，已连接的订阅者必须收到 session.updated，且**载荷内含权库视图**。
func TestSessionEventsUpdatedFrameCarriesSession(t *testing.T) {
	srv, bus, main, token := newSessionEventsEnv(t)
	r, cancel := openSessionSSE(t, srv, token)
	defer cancel()
	readSessionFrames(t, r, 1) // 吃掉首帧快照

	seedSession(t, main, "s-2", "新对话")
	bus.PublishImmediate("s-2", sessionbus.EventUpdated)

	frame := readSessionFrames(t, r, 1)[0]
	if frame.Event != sessionbus.EventUpdated {
		t.Fatalf("event 应为 session.updated，得到 %q", frame.Event)
	}
	var payload struct {
		Type    string        `json:"type"`
		Session model.Session `json:"session"`
	}
	if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
		t.Fatalf("updated 帧解析失败: %v", err)
	}
	// 关键断言：帧里带的是**回查数据库**后的完整视图，而非总线里那个空壳事件。
	if payload.Session.ID != "s-2" || payload.Session.Title != "新对话" {
		t.Fatalf("updated 帧应含回查后的会话视图，实际 %+v", payload.Session)
	}
}

// 删除会话后必须收到 session.removed（且只带 id，不查库）。
func TestSessionEventsRemovedFrame(t *testing.T) {
	srv, bus, main, token := newSessionEventsEnv(t)
	seedSession(t, main, "s-3", "待删")
	r, cancel := openSessionSSE(t, srv, token)
	defer cancel()
	readSessionFrames(t, r, 1)

	bus.PublishRemoved("s-3")

	frame := readSessionFrames(t, r, 1)[0]
	if frame.Event != sessionbus.EventRemoved {
		t.Fatalf("event 应为 session.removed，得到 %q", frame.Event)
	}
	var payload struct {
		Type      string `json:"type"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
		t.Fatalf("removed 帧解析失败: %v", err)
	}
	if payload.SessionID != "s-3" {
		t.Fatalf("removed 帧应含 session_id=s-3，实际 %+v", payload)
	}
}

// 会话已被删除时的 updated 事件必须**静默跳过**，不得误发 removed。
//
// 误发 removed 会让客户端永久摘掉一个仍存在的会话（比不更新更糟）。
func TestSessionEventsUpdatedForMissingSessionIsSkipped(t *testing.T) {
	srv, bus, _, token := newSessionEventsEnv(t)
	r, cancel := openSessionSSE(t, srv, token)
	defer cancel()
	readSessionFrames(t, r, 1)

	// 库里没有 s-ghost：推送该 id 的 updated 不应产生任何帧。
	bus.PublishImmediate("s-ghost", sessionbus.EventUpdated)

	if !framesQuiet(t, r, 300*time.Millisecond) {
		t.Fatal("不存在的会话不应产生任何帧（更不得误发 removed）")
	}
}

// framesQuiet 断言在这一段时间内**没有收到任何有效事件帧**。
//
// 实现要点（自己踩过的坑）：readSessionFrames 在读到 `data:` 行后立即返回，
// 帧尾的**分隔空行**仍留在缓冲里。若直接 Peek(1) 会立刻读到那个 '\n'，
// 让「本应静默」的用例假失败。故这里先跳过空白行，再判断是否有真实内容。
func framesQuiet(t *testing.T, r *bufio.Reader, d time.Duration) bool {
	t.Helper()
	// 150ms 用于等待「可能到来的帧」，其后 150ms 用于确认它真的没来。
	probe := time.After(d / 2)
	pending := make(chan bool, 1)
	go func() {
		for {
			b, err := r.Peek(1)
			if err != nil {
				pending <- false // 连接已断：也算静默（由其它断言负责）
				return
			}
			if b[0] == '\n' || b[0] == '\r' {
				_, _ = r.Discard(1) // 吃掉帧尾空行
				continue
			}
			pending <- true // 有真实内容
			return
		}
	}()
	select {
	case got := <-pending:
		return !got
	case <-probe:
		// 半个窗口内没有任何内容：再确认一段，仍无内容才算静默。
		select {
		case got := <-pending:
			return !got
		case <-time.After(d / 2):
			return true
		}
	}
}

// 未装配总线时必须返回可读的 unavailable，而不是挂起一个永不来的流。
func TestSessionEventsDisabledWhenNoBus(t *testing.T) {
	h := &handlers{d: Deps{SessionEvents: nil}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/sessions/events", h.sessionEvents)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/sessions/events")
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("应返回 503，得到 %d", resp.StatusCode)
	}
}

// 未鉴权访问必须被拒（该端点不在 publicPaths 内）。
func TestSessionEventsRequiresAuth(t *testing.T) {
	srv, _, _, _ := newSessionEventsEnv(t)
	resp, err := http.Get(srv.URL + "/api/v1/sessions/events")
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("未鉴权不应获得 SSE 流")
	}
}

// 字面量路由优先级：/sessions/events 不得被 {sid} 通配吃掉。
//
// 这条断言防的是「未来有人把注册顺序或匹配方式改坏」导致端点静默变成
// 「查询 id 为 events 的会话」—— 那时前端只会看到一个 404，极难定位。
func TestSessionEventsRouteNotShadowedBySessionID(t *testing.T) {
	srv, _, main, token := newSessionEventsEnv(t)
	seedSession(t, main, "s-1", "x")

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/sessions/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/sessions/events 应为 200（SSE），得到 %d —— 疑似被 {sid} 通配遮蔽", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("应为 SSE 流，得到 Content-Type=%q —— 说明命中了 {sid} 处理器", ct)
	}
}

// 创建会话接口必须真的广播（埋点回归）。
func TestCreateSessionPublishesEvent(t *testing.T) {
	srv, bus, _, token := newSessionEventsEnv(t)
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	body := strings.NewReader(`{"title":"实时新建"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/sessions", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("创建会话失败：%v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("创建会话应返回 201，得到 %d", resp.StatusCode)
	}

	select {
	case ev, open := <-sub.C():
		if !open {
			t.Fatal("订阅通道意外关闭")
		}
		if ev.Type != sessionbus.EventUpdated || ev.SessionID == "" {
			t.Fatalf("创建会话应广播 session.updated，实际 %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("创建会话后未收到广播（埋点遗漏或未在写库后触发）")
	}
}

// 删除会话接口必须广播 removed（而非 updated）。
func TestDeleteSessionPublishesRemoved(t *testing.T) {
	srv, bus, main, token := newSessionEventsEnv(t)
	sess := seedSession(t, main, "s-del", "待删")
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/sessions/"+sess.ID, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("删除会话失败：%v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	select {
	case ev, open := <-sub.C():
		if !open {
			t.Fatal("订阅通道意外关闭")
		}
		if ev.Type != sessionbus.EventRemoved || ev.SessionID != sess.ID {
			t.Fatalf("删除应广播 session.removed，实际 %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("删除会话后未收到广播")
	}
}
