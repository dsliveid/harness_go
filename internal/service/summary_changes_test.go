package service

import (
	"context"
	"testing"
	"time"

	"harness-go/internal/model"
	"harness-go/internal/store"
)

// fakeChangePublisher 记录 summaryWriter 发出的变更通知。
//
// 用假实现而非真 sessionbus：本测试关心的是「service 层在正确的时机、
// 以正确的通道发出通知」这一契约，不是总线的合并逻辑（那由 sessionbus 包自测）。
type fakeChangePublisher struct {
	updated  []string
	statused []string
}

func (f *fakeChangePublisher) PublishSessionUpdated(sessionID string) {
	f.updated = append(f.updated, sessionID)
}

func (f *fakeChangePublisher) PublishSessionStatusChanged(sessionID string) {
	f.statused = append(f.statused, sessionID)
}

// seedChangeSession 造一条会话供摘要回写。
func seedChangeSession(t *testing.T, main *store.MainStore, id string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := main.Sessions().Create(context.Background(), &model.Session{
		ID: id, Title: "t", AccessMode: "confirm", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("造会话失败: %v", err)
	}
}

// AddTokensAndStatus 必须在写库**成功之后**同时发出两条通知：
//   - PublishSessionUpdated（高频通道，可合并）→ 刷新 token 用量/消息时间；
//   - PublishSessionStatusChanged（低延迟通道）→ 刷新「运行中」徽标。
func TestSummaryWriterPublishesChangesAfterWrite(t *testing.T) {
	ctx := context.Background()
	main, err := store.OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	seedChangeSession(t, main, "s-1")

	pub := &fakeChangePublisher{}
	w := NewSummaryWriterWithChanges(main, pub)

	if err := w.AddTokensAndStatus(ctx, "s-1", 42, model.RunRunning); err != nil {
		t.Fatalf("回写失败: %v", err)
	}
	if len(pub.updated) != 1 || pub.updated[0] != "s-1" {
		t.Fatalf("应发出一次 updated 通知，实际 %+v", pub.updated)
	}
	if len(pub.statused) != 1 || pub.statused[0] != "s-1" {
		t.Fatalf("应发出一次 status 通知，实际 %+v", pub.statused)
	}

	// 终态也必须立即推：否则各端的「运行中」徽标会滞后消失。
	if err := w.AddTokensAndStatus(ctx, "s-1", 0, model.RunCompleted); err != nil {
		t.Fatalf("回写失败: %v", err)
	}
	if len(pub.statused) != 2 {
		t.Fatalf("终态也应立即推送，实际 status 通知数 %d", len(pub.statused))
	}

	// 通知发出时库里必须已是新值（验证「先写库后广播」的顺序契约）。
	sess, err := main.Sessions().Get(ctx, "s-1")
	if err != nil {
		t.Fatalf("查询会话失败: %v", err)
	}
	if sess.TotalTokens != 42 {
		t.Fatalf("广播时库内 token 应已为 42，实际 %d", sess.TotalTokens)
	}
}

// TouchMessage 也必须发出 updated（消息时间戳变化要反映到列表排序）。
func TestSummaryWriterTouchMessagePublishes(t *testing.T) {
	ctx := context.Background()
	main, err := store.OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	seedChangeSession(t, main, "s-2")

	pub := &fakeChangePublisher{}
	w := NewSummaryWriterWithChanges(main, pub)
	if err := w.TouchMessage(ctx, "s-2", time.Now().UnixMilli()); err != nil {
		t.Fatalf("回写失败: %v", err)
	}
	if len(pub.updated) != 1 || pub.updated[0] != "s-2" {
		t.Fatalf("应发出一次 updated 通知，实际 %+v", pub.updated)
	}
	// 消息时间戳不影响 Run 状态，不应误发 status 通知。
	if len(pub.statused) != 0 {
		t.Fatalf("TouchMessage 不应发 status 通知，实际 %+v", pub.statused)
	}
}

// 既有构造方式（不注入观察者）必须完全不变、且不 panic。
func TestSummaryWriterWithoutPublisherIsSafe(t *testing.T) {
	ctx := context.Background()
	main, err := store.OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	seedChangeSession(t, main, "s-3")

	w := NewSummaryWriter(main)
	if err := w.AddTokensAndStatus(ctx, "s-3", 7, model.RunCompleted); err != nil {
		t.Fatalf("无观察者时回写不应失败: %v", err)
	}
	if err := w.TouchMessage(ctx, "s-3", time.Now().UnixMilli()); err != nil {
		t.Fatalf("无观察者时回写不应失败: %v", err)
	}
	sess, err := main.Sessions().Get(ctx, "s-3")
	if err != nil {
		t.Fatalf("查询会话失败: %v", err)
	}
	if sess.TotalTokens != 7 {
		t.Fatalf("token 应已回写为 7，实际 %d", sess.TotalTokens)
	}
}
