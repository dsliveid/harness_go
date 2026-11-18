package service

import (
	"context"
	"testing"

	"harness-go/internal/model"
	"harness-go/internal/store"
)

// ============ 用户点「停止」后的服务端收口契约（2026-11 实测事故回归） ============
//
// 事故现象：
//   ① 界面已停，但 run_command 工具卡片一直显示「执行中…」；
//   ② 刷新（F5）后该对话又显示为「正在对话中」。
//
// 根因（两条，都在「记账复用被取消的 ctx」上）：
//   - RunService.StopRun 只 cancel(ctx)，Run 终态交给引擎 goroutine；
//   - 引擎收尾（TransitionRun / AddTokensAndStatus）与工具事件收口（FinishToolEvent）
//     复用被取消的 ctx → SQL 不执行、错误被 `_ =` 吞掉 → 库里永远 running / pending。
//
// 本文件锁死三条不变量（都在 StopRun 同步完成，不依赖引擎时序）：
//   1. Run 终态 = cancelled（→ /active-run 返回空 → 刷新后不再显示「正在对话中」）；
//   2. 主库投影 last_run_status = cancelled（→ 会话列表「运行中」徽标不再粘住）；
//   3. 未完成 tool_events 全部收口为 error（→ 卡片不再显示「执行中…」）。

// newStopRig 建真实主库 + 会话存储 + 一个进行中的 Run + 两条未完成工具事件。
func newStopRig(t *testing.T) (*RunService, *store.SessionStore, string, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	main, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })

	ss, err := store.OpenSessionStore(store.SessionStoreOptions{DataDir: dir, MaxOpen: 4})
	if err != nil {
		t.Fatalf("打开会话存储失败: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	svc := NewRunService(RunServiceOptions{Main: main, Sessions: ss})

	sid := "sess-stop"
	if err := main.Sessions().Create(ctx, &model.Session{ID: sid, Title: "停止", ModelRef: "fake-model"}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := main.Sessions().AddTokensAndStatus(ctx, sid, 0, model.RunRunning); err != nil {
		t.Fatalf("写投影失败: %v", err)
	}

	sdb, err := ss.Acquire(ctx, sid)
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	rid := "run-stop"
	if err := sdb.Runs().Create(ctx, &model.Run{ID: rid, ModelRef: "fake-model"}); err != nil {
		t.Fatalf("创建 Run 失败: %v", err)
	}
	if err := sdb.Runs().Transition(ctx, rid, model.RunRunning, "", "", ""); err != nil {
		t.Fatalf("置 running 失败: %v", err)
	}
	// 一条「执行中」的命令工具事件 + 一条 pending —— 停止后两条都必须收口。
	for _, id := range []string{"ev-running", "ev-pending"} {
		ev := &model.ToolEvent{ID: id, RunID: rid, ToolName: "run_command", Risk: model.RiskCommand}
		if id == "ev-running" {
			ev.Status = model.ToolRunning
		}
		if err := sdb.ToolEvents().Insert(ctx, ev); err != nil {
			t.Fatalf("插入工具事件失败: %v", err)
		}
	}
	return svc, ss, sid, rid
}

// TestStopRunSettlesRunAndProjection 停止后 Run 必须落 cancelled，主库投影同步。
func TestStopRunSettlesRunAndProjection(t *testing.T) {
	svc, ss, sid, rid := newStopRig(t)
	ctx := context.Background()

	// 模拟「引擎已注册取消句柄」：StopRun 必须真的把 ctx 取消掉（停止继续干活）。
	cancelled := 0
	cctx, cancel := context.WithCancel(ctx)
	svc.registerCancelForDevice(rid, "", func() { cancelled++; cancel() })
	_ = cctx

	if err := svc.StopRun(ctx, sid, rid); err != nil {
		t.Fatalf("StopRun 失败: %v", err)
	}
	if cancelled != 1 {
		t.Fatalf("StopRun 必须触发 ctx 取消一次，实际 %d", cancelled)
	}

	sdb, err := ss.Acquire(ctx, sid)
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	run, err := sdb.Runs().Get(ctx, rid)
	if err != nil {
		t.Fatalf("读取 Run 失败: %v", err)
	}
	if run.Status != model.RunCancelled {
		t.Fatalf("停止后 Run 应为 cancelled，得到 %s", run.Status)
	}
	if run.EndedAt == nil {
		t.Fatal("终态必须写 ended_at")
	}
	// /active-run 依据 ListUnfinished → 终态后必须查不到（这是「刷新后又显示进行中」的判据）。
	unfinished, err := sdb.Runs().ListUnfinished(ctx)
	if err != nil {
		t.Fatalf("ListUnfinished 失败: %v", err)
	}
	for _, r := range unfinished {
		if r.ID == rid {
			t.Fatal("停止后 /active-run 不得再命中该 Run")
		}
	}

	sess, err := svc.main.Sessions().Get(ctx, sid)
	if err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if sess.LastRunStatus != model.RunCancelled {
		t.Fatalf("主库投影应跟随之 cancelled，得到 %q", sess.LastRunStatus)
	}
}

// TestStopRunSettlesToolEvents 停止后未完成工具事件必须收口（卡片不再「执行中…」）。
func TestStopRunSettlesToolEvents(t *testing.T) {
	svc, ss, sid, rid := newStopRig(t)
	ctx := context.Background()

	if err := svc.StopRun(ctx, sid, rid); err != nil {
		t.Fatalf("StopRun 失败: %v", err)
	}
	sdb, err := ss.Acquire(ctx, sid)
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	evs, err := sdb.ToolEvents().ListByRun(ctx, rid)
	if err != nil {
		t.Fatalf("查询工具事件失败: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("应有 2 条工具事件，实际 %d", len(evs))
	}
	for _, ev := range evs {
		if ev.Status != model.ToolError {
			t.Fatalf("停止后工具事件必须收口为 error，得到 %s（%s）", ev.Status, ev.ID)
		}
		if ev.EndedAt == nil {
			t.Fatalf("收口必须写 ended_at（%s）", ev.ID)
		}
		if ev.Error == "" {
			t.Fatalf("收口必须写明原因（%s）", ev.ID)
		}
	}
}

// TestStopRunIsIdempotent 重复停止 / 引擎抢先收口都必须安全（前端不该看到「停止失败」）。
func TestStopRunIsIdempotent(t *testing.T) {
	svc, ss, sid, rid := newStopRig(t)
	ctx := context.Background()

	if err := svc.StopRun(ctx, sid, rid); err != nil {
		t.Fatalf("首次 StopRun 失败: %v", err)
	}
	// 第二次：Run 已是终态，取消句柄已注销 —— 仍必须返回 nil（幂等）。
	if err := svc.StopRun(ctx, sid, rid); err != nil {
		t.Fatalf("重复 StopRun 应幂等成功，得到 %v", err)
	}
	// 不存在的 Run：明确报错（不静默成功）。
	if err := svc.StopRun(ctx, sid, "run-nope"); err == nil {
		t.Fatal("不存在的 Run 必须报错")
	}
	// 空 run_id：参数错误。
	if err := svc.StopRun(ctx, sid, "  "); err == nil {
		t.Fatal("空 run_id 必须报错")
	}

	sdb, err := ss.Acquire(ctx, sid)
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	run, err := sdb.Runs().Get(ctx, rid)
	if err != nil {
		t.Fatalf("读取 Run 失败: %v", err)
	}
	if run.Status != model.RunCancelled {
		t.Fatalf("重复停止不得改写终态，得到 %s", run.Status)
	}
}
