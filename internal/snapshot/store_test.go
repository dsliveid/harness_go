package snapshot

// S10 快照存储单测：登记（锚点/去重/absent）→ 物理恢复 → 会话清理 + GC。

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"harness-go/internal/store"
	"harness-go/pkg/toolapi"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	main, err := store.OpenMain(ctx, dataDir)
	if err != nil {
		t.Fatalf("OpenMain: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	ws := t.TempDir()
	st, err := New(Options{Main: main, DataDir: dataDir, WorkspaceRoot: ws})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return st, ws
}

func mustWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读取 %s: %v", rel, err)
	}
	return string(data)
}

// TestSnapshotAnchoredCreateRestore 登记 → 修改 → 恢复（物理写回）。
func TestSnapshotAnchoredCreateRestore(t *testing.T) {
	st, ws := newTestStore(t)
	ctx := toolapi.WithCallInfo(context.Background(), toolapi.CallInfo{
		SessionID: "s1", RunID: "r1", ToolEventID: "e1",
	})
	mustWrite(t, ws, "a.txt", "v1")

	ref, err := st.CreateAnchored(ctx, []string{"a.txt"}, toolapi.SnapshotOpt{Kind: "before"})
	if err != nil {
		t.Fatalf("CreateAnchored: %v", err)
	}
	if ref.ID == "" || len(ref.Paths) != 1 || ref.Paths[0] != "a.txt" {
		t.Fatalf("快照引用异常: %+v", ref)
	}

	// 锚点落库（回滚服务按事件定位的依据）
	snap, err := st.Get(context.Background(), ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.SessionID != "s1" || snap.RunID != "r1" || snap.ToolEventID != "e1" || snap.Kind != "before" {
		t.Fatalf("锚点未落库: %+v", snap)
	}

	// 内容变更后恢复
	mustWrite(t, ws, "a.txt", "v2-changed")
	if err := st.Restore(context.Background(), ref); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := mustRead(t, ws, "a.txt"); got != "v1" {
		t.Fatalf("恢复失败: %q", got)
	}
}

// TestSnapshotAbsentPathRevertDeletes 快照时不存在 → 恢复即删除（文件创建的回滚方向）。
func TestSnapshotAbsentPathRevertDeletes(t *testing.T) {
	st, ws := newTestStore(t)
	ctx := toolapi.WithCallInfo(context.Background(), toolapi.CallInfo{SessionID: "s1"})

	ref, err := st.CreateAnchored(ctx, []string{"new.txt"}, toolapi.SnapshotOpt{Kind: "before"})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := st.Get(context.Background(), ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings.Paths) != 1 || !bindings.Paths[0].Absent {
		t.Fatalf("缺失文件应记 absent: %+v", bindings.Paths)
	}

	mustWrite(t, ws, "new.txt", "created")
	if err := st.Restore(context.Background(), toolapi.SnapshotRef{ID: ref.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("恢复 absent 应删除文件，err=%v", err)
	}
}

// TestSnapshotDedupAndGC 同内容去重 + 会话清理后 GC 物理回收。
func TestSnapshotDedupAndGC(t *testing.T) {
	st, ws := newTestStore(t)
	ctx := toolapi.WithCallInfo(context.Background(), toolapi.CallInfo{SessionID: "s1"})
	mustWrite(t, ws, "b.txt", "same-content")

	ref1, err := st.CreateAnchored(ctx, []string{"b.txt"}, toolapi.SnapshotOpt{Kind: "before"})
	if err != nil {
		t.Fatal(err)
	}
	// 第二个会话引用同内容（全局 CAS 去重）
	ctx2 := toolapi.WithCallInfo(context.Background(), toolapi.CallInfo{SessionID: "s2"})
	ref2, err := st.CreateAnchored(ctx2, []string{"b.txt"}, toolapi.SnapshotOpt{Kind: "before"})
	if err != nil {
		t.Fatal(err)
	}
	if ref1.Size != ref2.Size || ref1.Size == 0 {
		t.Fatalf("内容大小应一致: %d vs %d", ref1.Size, ref2.Size)
	}

	// 会话 1 清理：blob 仍被会话 2 引用，不回收
	freed, err := st.ForgetSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	n, err := st.GC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("仍有引用时不应回收: %d (freed=%v)", n, freed)
	}

	// 会话 2 也清理：引用归零 → GC 物理删除
	if _, err := st.ForgetSession(context.Background(), "s2"); err != nil {
		t.Fatal(err)
	}
	n, err = st.GC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应回收 1 个 blob，得到 %d", n)
	}
}

// TestSnapshotRejectsOutsidePaths 越界/绝对路径拒绝（与工作区沙箱同口径；
// 注：无盘符的 /abs 在 Windows 上按工作区内相对路径解析——与 hostimpl 一致，非越界）。
func TestSnapshotRejectsOutsidePaths(t *testing.T) {
	st, _ := newTestStore(t)
	for _, p := range []string{"../escape.txt", "..", "", "."} {
		if _, err := st.Create(context.Background(), []string{p}); err == nil {
			t.Fatalf("路径 %q 应被拒绝", p)
		}
	}
}

// TestSnapshotRestorePathSingle 单路径恢复原语（回滚服务的编排基础）。
func TestSnapshotRestorePathSingle(t *testing.T) {
	st, ws := newTestStore(t)
	ctx := toolapi.WithCallInfo(context.Background(), toolapi.CallInfo{SessionID: "s1"})
	mustWrite(t, ws, "x.txt", "old")
	ref, err := st.CreateAnchored(ctx, []string{"x.txt"}, toolapi.SnapshotOpt{Kind: "before"})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, ws, "x.txt", "new")

	res, err := st.RestorePath(context.Background(), ref.ID, "x.txt")
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "restored" || res.Bytes != int64(len("old")) {
		t.Fatalf("写回结果异常: %+v", res)
	}
	if got := mustRead(t, ws, "x.txt"); got != "old" {
		t.Fatalf("恢复失败: %q", got)
	}
	// 快照不含的路径必须显式报错
	if _, err := st.RestorePath(context.Background(), ref.ID, "other.txt"); err == nil {
		t.Fatal("快照不含的路径应报错")
	}
}
