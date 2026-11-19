package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"harness-go/internal/model"
	"harness-go/internal/sessionbus"
)

// 快照上限的边界契约。
//
// # 为什么这条测试必须存在（真实隐患）
//
// `store.SessionRepo.List` 里有一句防御：
//
//	if limit <= 0 || limit > 200 { limit = 50 }
//
// 而 SSE 快照传的恰好是 200 —— 也就是说**当前只是恰好安全**。
// 一旦有人把 `sessionEventsSnapshotLimit` 调成 201（或误以为是「无上限」），
// List 会**静默**把 limit 退化成 50，快照只推 50 条，且不报任何错。
// 表现是：用户会话多时，刷新后列表莫名只剩 50 条（而且每次重连都如此）。
//
// 这条测试把「快照 limit 必须 <= store 的硬上限」钉住，改坏即红。
func TestSessionEventsSnapshotLimitWithinStoreCap(t *testing.T) {
	// store 侧硬上限是 200（超过则退化为 50）。这两个数字必须满足 <=。
	const storeHardCap = 200
	if sessionEventsSnapshotLimit > storeHardCap {
		t.Fatalf("快照上限 %d 超过 store 硬上限 %d：List 会静默退化为 50 条，"+
			"表现为「会话多时刷新后只剩 50 条」且无任何报错",
			sessionEventsSnapshotLimit, storeHardCap)
	}
	if sessionEventsSnapshotLimit <= 0 {
		t.Fatalf("快照上限必须为正，实际 %d", sessionEventsSnapshotLimit)
	}
}

// 会话数超过上限时：快照必须被截断到上限，且**不报错**（不阻断实时流）。
//
// 设计取舍（已记录在 session_events_handler.go 的常量注释里）：
// 超出的会话不出现在快照里不是「丢数据」——列表刷新是覆盖式的，
// 真正的分页由前端显式翻页处理。但必须确认它不会变成 500 或者空帧。
func TestSessionEventsSnapshotTruncatesBeyondLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("造 201 条会话较慢，-short 下跳过")
	}
	srv, _, main, token := newSessionEventsEnv(t)
	ctx := context.Background()

	// 造 limit+1 条会话，确认快照被截断而非全量返回。
	total := sessionEventsSnapshotLimit + 1
	now := time.Now().UnixMilli()
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("s-bulk-%03d", i)
		if err := main.Sessions().Create(ctx, &model.Session{
			ID: id, Title: id, CreatedAt: now, UpdatedAt: now + int64(i),
		}); err != nil {
			t.Fatalf("造会话 %s 失败: %v", id, err)
		}
	}

	r, cancel := openSessionSSE(t, srv, token)
	defer cancel()

	frames := readSessionFrames(t, r, 1)
	if frames[0].Event != sessionbus.EventSnapshot {
		t.Fatalf("首帧应为 snapshot，得到 %q", frames[0].Event)
	}
	var payload struct {
		Items []model.Session `json:"items"`
	}
	if err := json.Unmarshal([]byte(frames[0].Data), &payload); err != nil {
		t.Fatalf("快照解析失败: %v", err)
	}
	if len(payload.Items) != sessionEventsSnapshotLimit {
		t.Fatalf("快照应截断到 %d 条，实际 %d 条（若为 %d 说明被 store 退化成 50）",
			sessionEventsSnapshotLimit, len(payload.Items), 50)
	}
}

// 增量帧不受快照上限影响：即使已有 200+ 会话，新建的第 201 条仍能实时推送。
//
// 这是「截断」是否影响实时性的关键验证 —— 若不成立，用户新建的会话
// 在会话数超过 200 后将永远看不到（直到手动刷新且它恰好排进前 200）。
func TestSessionEventsIncrementWorksBeyondSnapshotLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("造 200 条会话较慢，-short 下跳过")
	}
	srv, bus, main, token := newSessionEventsEnv(t)
	ctx := context.Background()

	now := time.Now().UnixMilli()
	for i := 0; i < sessionEventsSnapshotLimit; i++ {
		id := fmt.Sprintf("s-fill-%03d", i)
		if err := main.Sessions().Create(ctx, &model.Session{
			ID: id, Title: id, CreatedAt: now, UpdatedAt: now + int64(i),
		}); err != nil {
			t.Fatalf("造会话失败: %v", err)
		}
	}

	r, cancel := openSessionSSE(t, srv, token)
	defer cancel()
	readSessionFrames(t, r, 1) // 吃掉快照

	// 第 201 条（超出快照上限）新建 → 必须收到增量帧。
	newer := &model.Session{
		ID: "s-beyond", Title: "超出上限的新会话",
		CreatedAt: now, UpdatedAt: now + 10_000,
	}
	if err := main.Sessions().Create(ctx, newer); err != nil {
		t.Fatalf("造新会话失败: %v", err)
	}
	bus.PublishImmediate("s-beyond", sessionbus.EventUpdated)

	frame := readSessionFrames(t, r, 1)[0]
	if frame.Event != sessionbus.EventUpdated {
		t.Fatalf("event 应为 session.updated，得到 %q", frame.Event)
	}
	var payload struct {
		Session model.Session `json:"session"`
	}
	if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
		t.Fatalf("增量帧解析失败: %v", err)
	}
	if payload.Session.ID != "s-beyond" {
		t.Fatalf("增量帧应含 s-beyond，实际 %+v", payload.Session)
	}
}
