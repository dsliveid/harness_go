package service

// 阶段 G 服务层单测：长任务状态机 / 快照回滚（revert & reapply + 置灰）/ 会话导出包。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness-go/internal/model"
	"harness-go/internal/snapshot"
	"harness-go/internal/store"
	"harness-go/pkg/toolapi"
)

func newGEnv(t *testing.T) (*store.MainStore, *store.SessionStore, *snapshot.Store, string, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	main, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = main.Close() })
	sessions, err := store.OpenSessionStore(store.SessionStoreOptions{DataDir: dir, MaxOpen: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessions.Close() })
	wsRoot := t.TempDir()
	snaps, err := snapshot.New(snapshot.Options{Main: main, DataDir: dir, WorkspaceRoot: wsRoot})
	if err != nil {
		t.Fatal(err)
	}
	return main, sessions, snaps, wsRoot, dir
}

func mustSession(t *testing.T, main *store.MainStore, sessions *store.SessionStore, title string) string {
	t.Helper()
	ctx := context.Background()
	sess := &model.Session{ID: "sess-" + newID(), Title: title}
	if err := main.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Acquire(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	return sess.ID
}

func appendMsg(t *testing.T, sdb *store.SessionDB, role model.Role, content, runID string) *model.Message {
	t.Helper()
	m := &model.Message{ID: "m-" + newID(), Role: role, Content: content, RunID: runID, CreatedAt: store.NowMillis()}
	if err := sdb.Messages().Append(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m
}

// ============ 长任务 ============

// TestTaskManagerLifecycle 状态机：start → pause → resume → cancel（终态不可迁出）。
func TestTaskManagerLifecycle(t *testing.T) {
	main, sessions, snaps, _, _ := newGEnv(t)
	sid := mustSession(t, main, sessions, "长任务")
	mgr := NewTaskManager(TaskManagerOptions{Sessions: sessions, Snapshots: snaps})
	ctx := context.Background()

	task, err := mgr.Start(ctx, toolapi.TaskSpec{SessionID: sid, Title: "重构模块", RunID: "run-1",
		Meta: map[string]any{"paths": []string{"a.go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if task.State() != toolapi.TaskRunning {
		t.Fatalf("初始应 running，得到 %s", task.State())
	}
	if err := task.Pause(); err != nil {
		t.Fatal(err)
	}
	if task.State() != toolapi.TaskPaused {
		t.Fatalf("应 paused，得到 %s", task.State())
	}
	if err := task.Resume(); err != nil {
		t.Fatal(err)
	}
	if err := task.Cancel(); err != nil {
		t.Fatal(err)
	}
	if task.State() != toolapi.TaskCancelled {
		t.Fatalf("应 cancelled，得到 %s", task.State())
	}
	if err := task.Resume(); err == nil {
		t.Fatal("终态 resume 应拒绝")
	}
	sdb, err := sessions.Acquire(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	row, err := sdb.Tasks().Get(ctx, task.ID())
	if err != nil {
		t.Fatal(err)
	}
	if row.State != model.TaskCancelled || row.Title != "重构模块" {
		t.Fatalf("持久态异常: %+v", row)
	}
}

// TestTaskManagerCheckpointAtomic 检查点 = Run + 快照引用；未声明 paths 显式拒绝。
func TestTaskManagerCheckpointAtomic(t *testing.T) {
	main, sessions, snaps, _, _ := newGEnv(t)
	sid := mustSession(t, main, sessions, "检查点")
	mgr := NewTaskManager(TaskManagerOptions{Sessions: sessions, Snapshots: snaps})
	ctx := context.Background()

	noPaths, err := mgr.Start(ctx, toolapi.TaskSpec{SessionID: sid, Title: "无路径"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Checkpoint(ctx, noPaths.ID()); err == nil {
		t.Fatal("未声明 paths 应拒绝检查点")
	}

	task, err := mgr.Start(ctx, toolapi.TaskSpec{SessionID: sid, Title: "带路径", RunID: "run-2",
		Meta: map[string]any{"paths": []string{"work.txt"}}})
	if err != nil {
		t.Fatal(err)
	}
	cp, err := mgr.CheckpointNote(ctx, task.ID(), "第一步完成")
	if err != nil {
		t.Fatal(err)
	}
	if cp.SnapshotID == "" || cp.RunID != "run-2" || cp.Note != "第一步完成" {
		t.Fatalf("检查点异常: %+v", cp)
	}
	sdb, err := sessions.Acquire(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	cps, err := sdb.TaskCheckpoints().ListByTask(ctx, task.ID())
	if err != nil || len(cps) != 1 {
		t.Fatalf("检查点未落库: %v %v", cps, err)
	}
	snap, err := snaps.Get(ctx, cp.SnapshotID)
	if err != nil || snap.Kind != model.SnapshotKindCheckpoint || snap.TaskID != task.ID() {
		t.Fatalf("检查点快照锚点异常: %+v %v", snap, err)
	}
}

// TestTaskManagerCancelStopsRun Cancel 联动取消关联 Run。
func TestTaskManagerCancelStopsRun(t *testing.T) {
	main, sessions, snaps, _, _ := newGEnv(t)
	sid := mustSession(t, main, sessions, "取消联动")
	stopped := map[string]int{}
	mgr := NewTaskManager(TaskManagerOptions{
		Sessions:  sessions,
		Snapshots: snaps,
		RunStop: func(_ context.Context, sessID, runID string) error {
			stopped[sessID+"|"+runID]++
			return nil
		},
	})
	ctx := context.Background()
	task, err := mgr.Start(ctx, toolapi.TaskSpec{SessionID: sid, Title: "会取消", RunID: "run-9",
		Meta: map[string]any{"paths": []string{"x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Cancel(); err != nil {
		t.Fatal(err)
	}
	if stopped[sid+"|run-9"] != 1 {
		t.Fatalf("RunStop 应被调用一次: %v", stopped)
	}
}

// ============ 快照回滚 ============

// revertFixture 造一个「两次写入」的会话：事件 e1 创建 a.txt、事件 e2 创建 b.txt，
// 每个事件带 before（absent）/after 快照（模拟 fs 插件登记）。
func revertFixture(t *testing.T, ctx context.Context, main *store.MainStore, sessions *store.SessionStore, snaps *snapshot.Store, ws string) string {
	t.Helper()
	sid := mustSession(t, main, sessions, "回滚")
	sdb, err := sessions.Acquire(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	run := &model.Run{ID: "run-A", Status: model.RunCompleted}
	if err := sdb.Runs().Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	appendMsg(t, sdb, model.RoleUser, "改文件", "run-A")
	appendMsg(t, sdb, model.RoleAssistant, "好的", "run-A")

	for _, tc := range []struct {
		event, file, after string
	}{
		{"e1", "a.txt", "内容A"},
		{"e2", "b.txt", "内容B"},
	} {
		if _, err := os.Stat(filepath.Join(ws, tc.file)); err == nil {
			t.Fatalf("fixture 前置：%s 不应存在", tc.file)
		}
		ci := toolapi.WithCallInfo(ctx, toolapi.CallInfo{SessionID: sid, RunID: "run-A", ToolEventID: tc.event})
		if _, err := snaps.CreateAnchored(ci, []string{tc.file}, toolapi.SnapshotOpt{Kind: model.SnapshotKindBefore}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, tc.file), []byte(tc.after), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := snaps.CreateAnchored(ci, []string{tc.file}, toolapi.SnapshotOpt{Kind: model.SnapshotKindAfter}); err != nil {
			t.Fatal(err)
		}
	}
	return sid
}

// TestRollbackRevertTurnAndReapply 轮次级 revert（恢复 absent=删除文件）+ reapply（恢复 after）。
func TestRollbackRevertTurnAndReapply(t *testing.T) {
	main, sessions, snaps, ws, _ := newGEnv(t)
	ctx := context.Background()
	sid := revertFixture(t, ctx, main, sessions, snaps, ws)

	svc := NewRollbackService(RollbackServiceOptions{Main: main, Sessions: sessions, Snapshots: snaps})

	rb, err := svc.Revert(ctx, sid, RevertRequest{Mode: model.RollbackTurn, RunID: "run-A"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rb.Affected) != 2 {
		t.Fatalf("应覆盖 2 个工具事件: %d", len(rb.Affected))
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(ws, f)); !os.IsNotExist(err) {
			t.Fatalf("revert 后 %s 应被删除（快照时 absent）", f)
		}
	}
	if _, err := svc.Reapply(ctx, sid, rb.ID); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(ws, "a.txt"))
	if err != nil || string(data) != "内容A" {
		t.Fatalf("reapply 应恢复 after 内容: %q %v", data, err)
	}
	if _, err := svc.Reapply(ctx, sid, rb.ID); err == nil {
		t.Fatal("重复 reapply 应拒绝")
	}
}

// TestRollbackMessageHides 消息级回滚：置灰回滚点之后的消息 + reapply 恢复可见。
func TestRollbackMessageHides(t *testing.T) {
	main, sessions, snaps, ws, _ := newGEnv(t)
	ctx := context.Background()
	sid := revertFixture(t, ctx, main, sessions, snaps, ws)
	sdb, err := sessions.Acquire(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewRollbackService(RollbackServiceOptions{Main: main, Sessions: sessions, Snapshots: snaps})
	rb, err := svc.Revert(ctx, sid, RevertRequest{Mode: model.RollbackMessage, MessageSeq: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rb.HiddenFromSeq != 1 {
		t.Fatalf("置灰起点应为 1: %d", rb.HiddenFromSeq)
	}
	all, err := sdb.Messages().List(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	visible, err := sdb.Messages().ListVisible(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 || len(visible) != 0 {
		t.Fatalf("全部置灰但保留: all=%d visible=%d", len(all), len(visible))
	}
	if _, err := svc.Reapply(ctx, sid, rb.ID); err != nil {
		t.Fatal(err)
	}
	visible, err = sdb.Messages().ListVisible(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != len(all) {
		t.Fatalf("reapply 后应恢复可见: %d vs %d", len(visible), len(all))
	}
	_ = ws
}

// TestRollbackToolSingle 工具级：只回滚单个事件的路径。
func TestRollbackToolSingle(t *testing.T) {
	main, sessions, snaps, ws, _ := newGEnv(t)
	ctx := context.Background()
	sid := revertFixture(t, ctx, main, sessions, snaps, ws)

	svc := NewRollbackService(RollbackServiceOptions{Main: main, Sessions: sessions, Snapshots: snaps})
	if _, err := svc.Revert(ctx, sid, RevertRequest{Mode: model.RollbackTool, ToolEventID: "e1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("e1 回滚后 a.txt 应删除")
	}
	if data, err := os.ReadFile(filepath.Join(ws, "b.txt")); err != nil || string(data) != "内容B" {
		t.Fatalf("e2 不受影响: %q %v", data, err)
	}
}

// ============ 会话导出包 ============

// TestBundleExportImportRoundtrip 导出 → 导入恢复为新会话（消息/Run/事件/blob 原样）。
func TestBundleExportImportRoundtrip(t *testing.T) {
	main, sessions, _, _, dataDir := newGEnv(t)
	ctx := context.Background()
	sid := mustSession(t, main, sessions, "导出")
	sdb, err := sessions.Acquire(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	run := &model.Run{ID: "run-X", Status: model.RunCompleted}
	if err := sdb.Runs().Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	appendMsg(t, sdb, model.RoleUser, "你好", "run-X")
	appendMsg(t, sdb, model.RoleAssistant, "答", "run-X")
	ev := &model.ToolEvent{ID: "te-1", RunID: "run-X", ToolName: "write_file", Status: model.ToolOK,
		ArgsJSON: []byte(`{"path":"a.txt"}`)}
	if err := sdb.ToolEvents().Insert(ctx, ev); err != nil {
		t.Fatal(err)
	}
	cas, err := store.NewSessionBlobs(sessions.BlobRoot(sid), filepath.Join(dataDir, "tmp"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := cas.Put(ctx, strings.NewReader("blob-content-123"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	appendMsg(t, sdb, model.RoleUser, "blobref:"+ref.Hash, "run-X")

	svc := NewBundleService(BundleServiceOptions{
		Main: main, Sessions: sessions, DataDir: dataDir, WorkspaceRoot: t.TempDir(),
	})
	info, err := svc.Export(ctx, sid)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if info.Messages != 3 || info.Size == 0 {
		t.Fatalf("导出信息异常: %+v", info)
	}

	imported, err := svc.Import(ctx, info.Path)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if imported.SessionID == sid || imported.Messages != 3 {
		t.Fatalf("导入信息异常: %+v", imported)
	}
	newDB, err := sessions.Acquire(ctx, imported.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := newDB.Messages().List(ctx, 0, 100)
	if err != nil || len(msgs) != 3 {
		t.Fatalf("导入消息数异常: %d %v", len(msgs), err)
	}
	if msgs[2].Content != "blobref:"+ref.Hash {
		t.Fatalf("blobref 应原样保留: %q", msgs[2].Content)
	}
	runs, err := newDB.Runs().List(ctx)
	if err != nil || len(runs) != 1 || runs[0].ID != "run-X" {
		t.Fatalf("导入 Run 异常: %+v %v", runs, err)
	}
	content, err := os.ReadFile(filepath.Join(sessions.BlobRoot(imported.SessionID), ref.Hash[:2], ref.Hash))
	if err != nil || string(content) != "blob-content-123" {
		t.Fatalf("导入 blob 内容异常: %q %v", content, err)
	}
	// 导入路径沙箱：data 目录外绝对路径拒绝
	if _, err := svc.Import(ctx, filepath.Join(t.TempDir(), "evil.zip")); err == nil {
		t.Fatal("data 目录外的导入路径应拒绝")
	}
}
