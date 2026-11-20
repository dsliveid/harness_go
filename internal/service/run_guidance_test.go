package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// 运行中引导的**归属校验**（P0-1）：Run 必须属于 URL 指定的会话。
//
// 事故背景（实测）：前端 useChat 的 currentRunId / streaming 是模块级单例，切会话
// 不重置。「A 会话 Run 进行中 → 新建 B 会话发消息」会把引导 POST 到
// /sessions/{B}/runs/{A的run}/guidance。旧实现只按 runID 命中通道，然后拿 B 的
// 会话库 Append —— B 库里留下一条 run_id 属于 A 的孤儿 user 消息（runs 表为空、
// 永无回复），同时该内容被 A 的引擎回灌进历史尾部污染前缀缓存。
//
// 故本组测试锁死两条契约：
//   ① 跨会话引导 → 409 agent.run_session_mismatch，且**零落库**；
//   ② 同会话引导不受影响（消息落库 + seq 正常），防止校验写成「一律拒绝」。

// guidanceEnv 引导测试环境（fake provider + 可控延迟，保证 Run 在途可被引导）。
type guidanceEnv struct {
	svc      *RunService
	main     *store.MainStore
	sessions *store.SessionStore
	ctx      context.Context
}

func newGuidanceEnv(t *testing.T, delay time.Duration) *guidanceEnv {
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
	bus := events.NewBus(100, 32)
	t.Cleanup(bus.Close)

	provider := llm.NewFakeProvider()
	provider.Delay = delay
	engine, err := newRunAdmissionEngine(t, main, sessions, bus, provider)
	if err != nil {
		t.Fatalf("构造引擎失败: %v", err)
	}
	svc := NewRunService(RunServiceOptions{
		Main: main, Engine: engine, Sessions: sessions, Events: bus,
	})
	return &guidanceEnv{svc: svc, main: main, sessions: sessions, ctx: ctx}
}

// seedServerSession 造一条服务端内置空间的会话（不受任何设备在线性影响）。
func (e *guidanceEnv) seedServerSession(t *testing.T, id, title string) {
	t.Helper()
	sess := &model.Session{
		ID: id, Title: title,
		WorkspaceID: store.BuiltinWorkspaceServerID, AccessMode: "full", ModelRef: "fake-model",
	}
	if err := e.main.Sessions().Create(e.ctx, sess); err != nil {
		t.Fatalf("创建会话 %s 失败: %v", id, err)
	}
}

// messages 会话库内的消息（按 seq 升序）。
func (e *guidanceEnv) messages(t *testing.T, sessionID string) []model.Message {
	t.Helper()
	sdb, err := e.sessions.Acquire(e.ctx, sessionID)
	if err != nil {
		t.Fatalf("打开会话库 %s 失败: %v", sessionID, err)
	}
	msgs, err := sdb.Messages().List(e.ctx, 0, 500)
	if err != nil {
		t.Fatalf("读取消息 %s 失败: %v", sessionID, err)
	}
	return msgs
}

// waitGuidanceGone 等待引导通道注销（引擎 goroutine 收尾），避免用例结束时 DB 已关。
func (e *guidanceEnv) waitGuidanceGone(t *testing.T, runID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := e.svc.guidanceChans.Load(runID); !ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("Run %s 引导通道未在超时内注销（不致命）", runID)
}

// TestGuidanceRejectsCrossSession 跨会话引导必须 409，且错的会话库**零落库**。
func TestGuidanceRejectsCrossSession(t *testing.T) {
	env := newGuidanceEnv(t, 500*time.Millisecond)
	env.seedServerSession(t, "sess-guid-a", "A 会话")
	env.seedServerSession(t, "sess-guid-b", "B 会话")

	run, err := env.svc.CreateRun(env.ctx, "sess-guid-a", "你好", "fake-model")
	if err != nil {
		t.Fatalf("发起 Run 失败: %v", err)
	}
	t.Cleanup(func() {
		_ = env.svc.StopRun(context.Background(), "sess-guid-a", run.ID)
		env.waitGuidanceGone(t, run.ID)
	})

	// 通道已同步注册且**绑定 owner 会话**（注册发生在 goroutine 启动之前）。
	v, ok := env.svc.guidanceChans.Load(run.ID)
	if !ok {
		t.Fatal("Run 创建后引导通道应已注册")
	}
	entry, _ := v.(guidanceEntry)
	if entry.sessionID != "sess-guid-a" {
		t.Fatalf("引导通道应绑定 owner 会话 sess-guid-a，实际 %q", entry.sessionID)
	}

	// 用**错的会话 id** 引导：必须 409 + agent.run_session_mismatch。
	msg, err := env.svc.Guidance(env.ctx, "sess-guid-b", run.ID, "跨会话引导")
	if err == nil {
		t.Fatalf("跨会话引导应被拒绝，却成功了: %+v", msg)
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("错误应为 *apperr.Error，得到 %T: %v", err, err)
	}
	if ae.Code != apperr.CodeRunSessionMismatch {
		t.Fatalf("错误码应为 agent.run_session_mismatch，得到 %q（msg=%q）", ae.Code, ae.Message)
	}
	if code := ae.Status(); code != 409 {
		t.Fatalf("该错误码应映射 409，得到 %d", code)
	}

	// 核心断言：B 会话库**必须仍为 0 条消息**（防回归的第一道闸）。
	if got := env.messages(t, "sess-guid-b"); len(got) != 0 {
		t.Fatalf("被拒的跨会话引导不得落库，B 会话却有 %d 条消息: %+v", len(got), got)
	}
	// A 会话也不该收到这条引导（只有 CreateRun 写的那条 user 消息）。
	for _, m := range env.messages(t, "sess-guid-a") {
		if m.Content == "跨会话引导" {
			t.Fatalf("被拒的引导不得出现在任何会话库中，A 会话却出现了: %+v", m)
		}
	}
}

// TestGuidanceSameSessionOK 同会话引导仍成功：消息落库、seq 递增、RunID 归属正确。
func TestGuidanceSameSessionOK(t *testing.T) {
	env := newGuidanceEnv(t, 500*time.Millisecond)
	env.seedServerSession(t, "sess-guid-same", "同会话")

	run, err := env.svc.CreateRun(env.ctx, "sess-guid-same", "你好", "fake-model")
	if err != nil {
		t.Fatalf("发起 Run 失败: %v", err)
	}
	t.Cleanup(func() {
		_ = env.svc.StopRun(context.Background(), "sess-guid-same", run.ID)
		env.waitGuidanceGone(t, run.ID)
	})

	before := env.messages(t, "sess-guid-same")
	msg, err := env.svc.Guidance(env.ctx, "sess-guid-same", run.ID, "同会话引导")
	if err != nil {
		t.Fatalf("同会话引导应成功，却失败: %v", err)
	}
	if msg == nil || msg.ID == "" {
		t.Fatal("应返回落库消息")
	}
	if msg.RunID != run.ID {
		t.Fatalf("消息 RunID 应归属该 Run，得到 %q（期望 %q）", msg.RunID, run.ID)
	}
	if msg.Content != "同会话引导" || msg.Role != model.RoleUser {
		t.Fatalf("落库消息内容/角色错误: %+v", msg)
	}

	after := env.messages(t, "sess-guid-same")
	if len(after) != len(before)+1 {
		t.Fatalf("同会话引导应新增恰好 1 条消息：前 %d 后 %d", len(before), len(after))
	}
	last := after[len(after)-1]
	if last.ID != msg.ID {
		t.Fatalf("最新消息应为引导消息，得到 %+v", last)
	}
	if prev := before[len(before)-1]; last.Seq <= prev.Seq {
		t.Fatalf("seq 必须递增：前 %d 后 %d", prev.Seq, last.Seq)
	}
}
