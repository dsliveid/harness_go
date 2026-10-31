package main

// 阶段 G 出口门禁（docs/开发实施计划.md §5-G）：
//
//   - **回滚与文件写事务一致**：真实引擎发起 write_file → fs 插件先登记 before
//     快照再落盘 → after 快照 → 回滚服务物理写回（turn 级恢复 absent=删除），
//     reapply 恢复 after；消息级回滚置灰回滚点之后的消息。
//   - **长任务检查点可用**：start_long_task → checkpoint_long_task 生成
//     「Run + 快照引用」原子登记（会话库检查点行 + 主库快照行 kind=checkpoint），
//     快照可物理恢复。
//   - **SOP 全量注入**（D10）：save_sop 落 .harness/sop/ → 装配点读出进 prompt。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"harness-go/internal/agent"
	"harness-go/internal/approval"
	"harness-go/internal/llm"
	"harness-go/internal/model"
	"harness-go/internal/plugins/longtask"
	"harness-go/internal/plugins/sop"
	"harness-go/internal/service"
	"harness-go/pkg/toolapi"
)

// newGEnv 在 fs_approval 测试环境基础上追加：long-task 插件 + 长任务状态机 + 回滚服务。
func newGEnv(t *testing.T, rounds [][]llm.Chunk) (*fsApprovalEnv, *service.TaskManager, *service.RollbackService) {
	t.Helper()
	env := newFSApprovalEnv(t, time.Second, rounds)
	if err := env.toolReg.RegisterFactory(context.Background(), longtask.Factory(), env.toolHost, nil); err != nil {
		t.Fatalf("long-task 插件装配失败: %v", err)
	}
	mgr := service.NewTaskManager(service.TaskManagerOptions{
		Sessions:  env.sessions,
		Snapshots: env.snaps,
	})
	env.toolHost.SetTasks(mgr)
	rbSvc := service.NewRollbackService(service.RollbackServiceOptions{
		Main:      env.main,
		Sessions:  env.sessions,
		Snapshots: env.snaps,
	})
	return env, mgr, rbSvc
}

// runEngine 预建 Run 行并同步执行（成功路径）。
func runEngine(t *testing.T, env *fsApprovalEnv, runID, input string) {
	t.Helper()
	sdb0, err := env.sessions.Acquire(context.Background(), env.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sdb0.Runs().Create(context.Background(), &model.Run{
		ID: runID, Status: model.RunPending, ModelRef: "test-model", Executor: model.ExecutorServer,
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Execute(context.Background(), agent.RunRequest{
		RunID: runID, SessionID: env.sessionID, UserInput: input,
		ModelRef: "test-model", AccessMode: approval.ModeFull,
		SystemBase: "测试",
	}); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
}

// TestGRollbackConsistentWithWrite 出口门禁①：写入-快照-回滚与文件写事务一致。
func TestGRollbackConsistentWithWrite(t *testing.T) {
	env, _, rbSvc := newGEnv(t, [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{ID: "call-1", Name: "write_file",
			ArgumentsDelta: `{"path":"g-report.md","content":"报告 v1\n"}`}}},
		{{Delta: "已写入"}, {Usage: &llm.Usage{TotalTokens: 3}}},
	})
	runEngine(t, env, "run-g1", "写报告")

	ctx := context.Background()
	// ① 文件已落盘
	data, err := os.ReadFile(filepath.Join(env.dataDir, "g-report.md"))
	if err != nil || string(data) != "报告 v1\n" {
		t.Fatalf("写入应生效: %q %v", data, err)
	}
	// ② 快照锚点落库：before（absent）+ after，绑定 tool_event
	sdb, err := env.sessions.Acquire(ctx, env.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := sdb.ToolEvents().ListByRun(ctx, "run-g1")
	if len(evs) != 1 {
		t.Fatalf("应有 1 个工具事件: %d", len(evs))
	}
	snaps, err := env.snaps.ListBySession(ctx, env.sessionID, 10)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, s := range snaps {
		if s.ToolEventID == evs[0].ID {
			kinds[s.Kind] = s.ID
		}
	}
	if kinds["before"] == "" || kinds["after"] == "" {
		t.Fatalf("写入事件应有 before/after 快照: %+v", snaps)
	}
	beforeSnap, _ := env.snaps.Get(ctx, kinds["before"])
	if len(beforeSnap.Paths) != 1 || !beforeSnap.Paths[0].Absent {
		t.Fatalf("before 快照应记 absent（新文件）: %+v", beforeSnap.Paths)
	}

	// ③ turn 级回滚：物理删除（恢复 absent）
	rb, err := rbSvc.Revert(ctx, env.sessionID, service.RevertRequest{Mode: model.RollbackTurn, RunID: "run-g1"})
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "g-report.md")); !os.IsNotExist(err) {
		t.Fatal("回滚后文件应物理删除（写事务一致）")
	}
	if len(rb.Restored) != 1 || rb.Restored[0].Action != "deleted" {
		t.Fatalf("写回结果应含 deleted: %+v", rb.Restored)
	}

	// ④ reapply：恢复 after 内容
	if _, err := rbSvc.Reapply(ctx, env.sessionID, rb.ID); err != nil {
		t.Fatalf("Reapply: %v", err)
	}
	data, err = os.ReadFile(filepath.Join(env.dataDir, "g-report.md"))
	if err != nil || string(data) != "报告 v1\n" {
		t.Fatalf("reapply 应恢复内容: %q %v", data, err)
	}

	// ⑤ 消息级回滚：置灰回滚点之后的消息（回滚点 = 用户消息 seq 1）
	rb2, err := rbSvc.Revert(ctx, env.sessionID, service.RevertRequest{Mode: model.RollbackMessage, MessageSeq: 1})
	if err != nil {
		t.Fatalf("消息级回滚: %v", err)
	}
	if rb2.HiddenFromSeq != 1 {
		t.Fatalf("置灰起点应为 1: %d", rb2.HiddenFromSeq)
	}
	visible, _ := sdb.Messages().ListVisible(ctx, 0, 100)
	if len(visible) != 0 {
		t.Fatalf("回滚点之后应全部置灰: %d", len(visible))
	}
}

// TestGLongTaskCheckpointUsable 出口门禁②：长任务检查点可用（Run + 快照引用）。
func TestGLongTaskCheckpointUsable(t *testing.T) {
	env, mgr, _ := newGEnv(t, [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{ID: "call-1", Name: "start_long_task",
			ArgumentsDelta: `{"title":"重构检索模块","paths":["src/index.go"]}`}}},
		{{ToolCall: &llm.ToolCallDelta{ID: "call-2", Name: "checkpoint_long_task",
			ArgumentsDelta: `{"task_id":"TASKID","note":"完成第一步"}`}}},
		{{Delta: "检查点已登记"}, {Usage: &llm.Usage{TotalTokens: 5}}},
	})
	// 预写检查点前先落任务（第二轮的 task_id 依赖第一轮结果——用固定 ID 占位替换）
	// 为让预设轮次可用，先直接经状态机登记固定 ID 任务：
	ctx := context.Background()
	fixed, err := mgr.Start(ctx, toolapi.TaskSpec{
		ID: "task-fixed", SessionID: env.sessionID, RunID: "run-g2",
		Title: "重构检索模块", Meta: map[string]any{"paths": []string{"src/index.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = fixed
	env.provider.rounds[1][0].ToolCall.ArgumentsDelta =
		`{"task_id":"task-fixed","note":"完成第一步"}`

	runEngine(t, env, "run-g2", "开始长任务")
	if !strings.Contains(env.provider.rounds[0][0].ToolCall.ArgumentsDelta, "重构检索模块") {
		t.Fatal("sanity: 预设轮次应含 start_long_task")
	}

	// ① 引擎内两次工具调用都成功
	sdb, err := env.sessions.Acquire(ctx, env.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := sdb.ToolEvents().ListByRun(ctx, "run-g2")
	if len(evs) != 2 {
		t.Fatalf("应有 2 个工具事件: %d", len(evs))
	}
	for _, ev := range evs {
		if ev.Status != model.ToolOK {
			t.Fatalf("工具事件应 ok: %+v", ev)
		}
	}
	// ② 检查点行落库 + 快照锚点（kind=checkpoint, task_id 关联）
	cps, err := sdb.TaskCheckpoints().ListByTask(ctx, "task-fixed")
	if err != nil || len(cps) != 1 {
		t.Fatalf("检查点未落库: %v %v", cps, err)
	}
	if cps[0].RunID != "run-g2" || cps[0].Note != "完成第一步" {
		t.Fatalf("检查点应含 Run 引用与备注: %+v", cps[0])
	}
	snap, err := env.snaps.Get(ctx, cps[0].SnapshotID)
	if err != nil || snap.Kind != model.SnapshotKindCheckpoint || snap.TaskID != "task-fixed" {
		t.Fatalf("检查点快照锚点异常: %+v %v", snap, err)
	}
	// ③ 检查点快照可物理恢复（先快照 v1 → 覆盖 → 恢复）
	if err := os.MkdirAll(filepath.Join(env.dataDir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 生成检查点时 src/index.go 尚不存在 → absent；先重建场景：写文件后再次检查点
	if err := os.WriteFile(filepath.Join(env.dataDir, "src", "index.go"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.CheckpointNote(ctx, "task-fixed", "第二步"); err != nil {
		t.Fatal(err)
	}
	cps, _ = sdb.TaskCheckpoints().ListByTask(ctx, "task-fixed")
	if len(cps) != 2 {
		t.Fatalf("应有 2 个检查点: %d", len(cps))
	}
	if err := env.snaps.Restore(ctx, toolapi.SnapshotRef{ID: cps[1].SnapshotID}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(env.dataDir, "src", "index.go"))
	if err != nil || string(data) != "v2\n" {
		t.Fatalf("检查点快照应可恢复: %q %v", data, err)
	}
	// ④ 回滚到第一个检查点（absent → 删除）
	if err := env.snaps.Restore(ctx, toolapi.SnapshotRef{ID: cps[0].SnapshotID}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, "src", "index.go")); !os.IsNotExist(err) {
		t.Fatal("回滚到首个检查点应删除 absent 文件")
	}
}

// TestGSOPInjectedIntoPrompt D10：save_sop 落盘 → 装配点读出（prompt.Input.SOP 源）。
func TestGSOPInjectedIntoPrompt(t *testing.T) {
	env, _, _ := newGEnv(t, nil)
	p, err := sop.Factory().New(env.toolHost, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range p.Tools() {
		if tool.Spec().Name != "save_sop" {
			continue
		}
		res, iErr := tool.Invoke(context.Background(), &toolapi.Call{
			CallID: "sop:1", Name: "save_sop",
			Args: []byte(`{"title":"部署规范","content":"1. 先跑 make verify"}`),
		})
		if iErr != nil {
			t.Fatal(iErr)
		}
		if res.IsError {
			t.Fatalf("save_sop 失败: %s", res.Content)
		}
	}
	bodies := sopBodies(env.toolHost)
	if len(bodies) != 1 || !strings.Contains(bodies[0], "make verify") {
		t.Fatalf("装配点应读出 SOP 正文: %+v", bodies)
	}
	// 超上限拒绝（不静默截断）
	big := strings.Repeat("冗", 300)
	for _, tool := range p.Tools() {
		if tool.Spec().Name != "save_sop" {
			continue
		}
		res, iErr := tool.Invoke(context.Background(), &toolapi.Call{
			CallID: "sop:2", Name: "save_sop",
			Args: []byte(`{"title":"超限条目","content":"` + big + `"}`),
		})
		if iErr != nil {
			t.Fatal(iErr)
		}
		if !res.IsError {
			t.Fatal("超单条上限应显式拒绝")
		}
	}
}
