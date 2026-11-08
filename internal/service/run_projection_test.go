package service

import (
	"context"
	"testing"

	"harness-go/internal/model"
	"harness-go/internal/store"
)

// ============ 主库投影跟随真相（单一真相铁律） ============
//
// 背景（实测事故）：sessions.last_run_status 是 session.db.runs.status 的**投影**，
// 但此前只在 Run 终态时由 SummaryWriter 回写 → Run 运行期间主库仍挂着上一轮 completed。
// 由此产生两处实测缺陷：
//   - ① RecoverInterrupted 按主库 last_run_status 筛选会话 → 漏扫 → 进程重启后
//     卡死在 running 的 Run 永远不会被置 interrupted（本次事故实证：重启后 83abf735 仍 running）；
//   - ② 前端会话列表「运行中」徽标判据同为 last_run_status，永不命中。
//
// 契约：Run 一创建，主库投影就必须是非终态。

// newProjectionRig 建真实主库 + 会话存储（不含引擎），用于验证投影写入。
func newProjectionRig(t *testing.T) (*RunService, *store.SessionStore, string) {
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

	sess := &model.Session{ID: "sess-proj", Title: "投影", ModelRef: "fake-model"}
	if err := main.Sessions().Create(ctx, sess); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	return svc, ss, sess.ID
}

// 重启恢复必须能扫到「会话库里有未完成 Run」的会话，即便主库投影曾被写成终态。
//
// 这是上一轮事故的直接回归：主库 last_run_status=completed + 会话库 runs.status=running
// 的组合曾让 RecoverInterrupted 直接跳过该会话，卡死 Run 永远留在 running。
func TestRecoverInterruptedCoversProjectedRunningSession(t *testing.T) {
	svc, ss, sid := newProjectionRig(t)
	ctx := context.Background()

	sdb, err := ss.Acquire(ctx, sid)
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	// 模拟运行中的 Run（pending → running），并把主库投影同步为非终态
	rid := "run-proj"
	if err := sdb.Runs().Create(ctx, &model.Run{ID: rid, ModelRef: "fake-model"}); err != nil {
		t.Fatalf("创建 Run 失败: %v", err)
	}
	if err := sdb.Runs().Transition(ctx, rid, model.RunRunning, "", "", ""); err != nil {
		t.Fatalf("置 running 失败: %v", err)
	}
	if err := svc.main.Sessions().AddTokensAndStatus(ctx, sid, 0, model.RunRunning); err != nil {
		t.Fatalf("写投影失败: %v", err)
	}

	// 投影确实是非终态（契约前提）
	sess, err := svc.main.Sessions().Get(ctx, sid)
	if err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if !sess.LastRunStatus.IsUnfinished() {
		t.Fatalf("主库投影应为非终态，得到 %q", sess.LastRunStatus)
	}

	// 重启恢复：必须把该 Run 置 interrupted
	n, err := svc.RecoverInterrupted(ctx)
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应恢复 1 个未完成 Run，实际 %d", n)
	}
	run, err := sdb.Runs().Get(ctx, rid)
	if err != nil {
		t.Fatalf("读取 Run 失败: %v", err)
	}
	if run.Status != model.RunInterrupted {
		t.Fatalf("重启后应置 interrupted，得到 %s", run.Status)
	}
}

// 审批等待中的 Run 也属「未完成」：重启后必须被收口为 interrupted，
// 否则 waiting_approval 会成为跨进程悬挂态（前端会一直等一个不会再来的审批）。
func TestRecoverInterruptedCoversWaitingApproval(t *testing.T) {
	svc, ss, sid := newProjectionRig(t)
	ctx := context.Background()

	sdb, err := ss.Acquire(ctx, sid)
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	rid := "run-wait"
	if err := sdb.Runs().Create(ctx, &model.Run{ID: rid, ModelRef: "fake-model"}); err != nil {
		t.Fatalf("创建 Run 失败: %v", err)
	}
	if err := sdb.Runs().Transition(ctx, rid, model.RunRunning, "", "", ""); err != nil {
		t.Fatalf("置 running 失败: %v", err)
	}
	if err := sdb.Runs().Transition(ctx, rid, model.RunWaitingApproval, "", "", ""); err != nil {
		t.Fatalf("置 waiting_approval 失败: %v", err)
	}
	if err := svc.main.Sessions().AddTokensAndStatus(ctx, sid, 0, model.RunWaitingApproval); err != nil {
		t.Fatalf("写投影失败: %v", err)
	}

	if _, err := svc.RecoverInterrupted(ctx); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	run, err := sdb.Runs().Get(ctx, rid)
	if err != nil {
		t.Fatalf("读取 Run 失败: %v", err)
	}
	if run.Status != model.RunInterrupted {
		t.Fatalf("等待审批的 Run 重启后也应置 interrupted，得到 %s", run.Status)
	}
}
