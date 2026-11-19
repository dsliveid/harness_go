package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"harness-go/internal/model"
)

// 未读红点的端到端契约：列表返回 unread → POST /read → 再查变已读。
//
// 这条链路横跨 service 派生与 store 游标，任一层写错都会导致
// 「红点永不出现」或「红点永不消失」，而两者都不会报错，必须用测试钉住。
func TestSessionUnreadClearedByMarkRead(t *testing.T) {
	srv, _, main, token := newSessionEventsEnv(t)
	ctx := context.Background()

	// 造一条「有消息但从未上报已读」的会话 → unread 应为 true。
	now := time.Now().UnixMilli()
	if err := main.Sessions().Create(ctx, &model.Session{ID: "s-unread", Title: "有未读", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := main.Sessions().TouchMessage(ctx, "s-unread", now); err != nil {
		t.Fatalf("回写消息失败: %v", err)
	}

	// ① 初始状态：unread = true（游标 0 < 最后消息时间）。
	var list struct {
		Items []model.Session `json:"items"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions", token, nil, http.StatusOK, &list)
	if len(list.Items) != 1 {
		t.Fatalf("应列出 1 条会话，实际 %d", len(list.Items))
	}
	if !list.Items[0].Unread {
		t.Fatalf("有消息且未上报已读时应为未读，实际 %+v", list.Items[0])
	}

	// ② 上报已读 → 204。
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions/s-unread/read", token, nil, http.StatusNoContent, nil)

	// ③ 再查：unread = false，游标推进到最后消息时间。
	var got model.Session
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/s-unread", token, nil, http.StatusOK, &got)
	if got.Unread {
		t.Fatalf("上报已读后应变为已读，实际 %+v", got)
	}
	if got.LastReadAt != now {
		t.Fatalf("游标应等于最后消息时间 %d，实际 %d", now, got.LastReadAt)
	}
}

// 新消息到达后必须重新变回未读（否则红点只会消失一次，之后彻底失效）。
func TestSessionUnreadReappearsAfterNewMessage(t *testing.T) {
	srv, _, main, token := newSessionEventsEnv(t)
	ctx := context.Background()

	now := time.Now().UnixMilli()
	if err := main.Sessions().Create(ctx, &model.Session{ID: "s-again", Title: "t", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := main.Sessions().TouchMessage(ctx, "s-again", now); err != nil {
		t.Fatalf("回写消息失败: %v", err)
	}
	// 先读一次，清掉未读。
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions/s-again/read", token, nil, http.StatusNoContent, nil)

	// 别端随后又发了新消息（时间更晚）。
	later := now + 60_000
	if err := main.Sessions().TouchMessage(ctx, "s-again", later); err != nil {
		t.Fatalf("回写第二条消息失败: %v", err)
	}

	var got model.Session
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/s-again", token, nil, http.StatusOK, &got)
	if !got.Unread {
		t.Fatalf("收到更晚的新消息后应重新变为未读，实际 unread=%v lastRead=%d", got.Unread, got.LastReadAt)
	}
}

// 空会话（从未有消息）不得显示未读红点。
func TestSessionUnreadFalseForEmptySession(t *testing.T) {
	srv, _, main, token := newSessionEventsEnv(t)
	now := time.Now().UnixMilli()
	if err := main.Sessions().Create(context.Background(), &model.Session{ID: "s-empty", Title: "空", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	var got model.Session
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/s-empty", token, nil, http.StatusOK, &got)
	if got.Unread {
		t.Fatalf("空会话不应为未读，实际 %+v", got)
	}
}

// 已读上报应广播 session.updated（跨端红点同步）。
func TestMarkReadPublishesSessionEvent(t *testing.T) {
	srv, bus, main, token := newSessionEventsEnv(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if err := main.Sessions().Create(ctx, &model.Session{ID: "s-pub", Title: "t", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := main.Sessions().TouchMessage(ctx, "s-pub", now); err != nil {
		t.Fatalf("回写消息失败: %v", err)
	}

	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions/s-pub/read", token, nil, http.StatusNoContent, nil)

	select {
	case ev := <-sub.C():
		if ev.SessionID != "s-pub" {
			t.Fatalf("广播的会话 id 不符: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("已读上报后未收到广播（其他端红点无法同步消失）")
	}
}

// 对不存在的会话上报已读必须 404，而不是静默成功。
func TestMarkReadUnknownSessionNotFound(t *testing.T) {
	srv, _, _, token := newSessionEventsEnv(t)
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions/no-such/read", token, nil, http.StatusNotFound, nil)
}

// 未鉴权不得上报已读（避免匿名请求改他人已读状态）。
func TestMarkReadRequiresAuth(t *testing.T) {
	srv, _, main, _ := newSessionEventsEnv(t)
	now := time.Now().UnixMilli()
	if err := main.Sessions().Create(context.Background(), &model.Session{ID: "s-auth", Title: "t", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	resp, err := http.Post(srv.URL+"/api/v1/sessions/s-auth/read", "application/json", nil)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		t.Fatal("未鉴权不应能上报已读")
	}
}
