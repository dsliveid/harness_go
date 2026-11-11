package service

import (
	"context"
	"errors"
	"os"
	"testing"

	"harness-go/internal/apperr"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// ============ 工作空间删除：service 层的 migrateRefs 透传与错误映射 ============
//
// 用户的诉求是「点了确认就直接删除」，而不是"删了刷新又出现"。这要求
// service.Delete(migrateRefs=true) 真的走到 store.DeleteMigratingRefs，
// 且内置空间/不存在这两种情形的错误码不能被吞成 500。

// newWorkspaceSvc 造一个只带主库的 WorkspaceService 测试环境。
func newWorkspaceSvc(t *testing.T) (context.Context, *WorkspaceService, *store.MainStore) {
	t.Helper()
	ctx := context.Background()
	main, err := store.OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	return ctx, NewWorkspaceService(main, nil), main
}

// seedWsWithSession 造一个带会话引用的非内置空间（模拟真实场景：空间下必然有会话）。
func seedWsWithSession(t *testing.T, ctx context.Context, main *store.MainStore, wsID string) {
	t.Helper()
	if err := main.Workspaces().Create(ctx, &model.Workspace{
		ID: wsID, Name: wsID, Kind: model.WorkspaceKindDesktop,
	}); err != nil {
		t.Fatalf("创建空间失败: %v", err)
	}
	if err := main.Sessions().Create(ctx, &model.Session{
		ID: "s-" + wsID, Title: "会话", WorkspaceID: wsID,
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
}

// migrateRefs=true：有会话引用也能删掉（用户点确认后的真实行为），且会话被级联删除。
func TestWorkspaceDeleteMigrateRefsSucceeds(t *testing.T) {
	ctx, svc, main := newWorkspaceSvc(t)
	seedWsWithSession(t, ctx, main, "ws-web")

	if err := svc.Delete(ctx, "ws-web", true); err != nil {
		t.Fatalf("migrateRefs=true 应允许删除带引用的空间，得到 %v", err)
	}
	if _, err := main.Workspaces().Get(ctx, "ws-web"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("空间应已物理删除（刷新不应复活），得到 %v", err)
	}
	// 级联语义：会话也必须一并删除（不是保留、也不是解除归属到别处）。
	if _, err := main.Sessions().Get(ctx, "s-ws-web"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("会话应被级联删除，得到 %v", err)
	}
}

// 严格模式（migrateRefs=false）：保留旧契约 —— 有引用即 409。
func TestWorkspaceDeleteStrictMapsConflict(t *testing.T) {
	ctx, svc, main := newWorkspaceSvc(t)
	seedWsWithSession(t, ctx, main, "ws-strict")

	err := svc.Delete(ctx, "ws-strict", false)
	if err == nil {
		t.Fatal("严格模式下带引用应报错")
	}
	if !isCode(err, apperr.CodeConflict) {
		t.Fatalf("应映射为 409 conflict，得到 %v", err)
	}
}

// 内置服务端空间：两种模式都必须 409，绝不能被删。
func TestWorkspaceDeleteBuiltinIsConflict(t *testing.T) {
	ctx, svc, main := newWorkspaceSvc(t)
	for _, migrate := range []bool{false, true} {
		err := svc.Delete(ctx, store.BuiltinWorkspaceServerID, migrate)
		if !isCode(err, apperr.CodeConflict) {
			t.Fatalf("migrateRefs=%v 时内置空间应 409，得到 %v", migrate, err)
		}
	}
	if _, err := main.Workspaces().Get(ctx, store.BuiltinWorkspaceServerID); err != nil {
		t.Fatalf("内置空间应仍存在，却查不到: %v", err)
	}
}

// 不存在的 ID：两种模式都应 404（而不是 500 或假成功）。
func TestWorkspaceDeleteNotFoundIs404(t *testing.T) {
	ctx, svc, _ := newWorkspaceSvc(t)
	for _, migrate := range []bool{false, true} {
		err := svc.Delete(ctx, "ws-ghost", migrate)
		if !isCode(err, apperr.CodeNotFound) {
			t.Fatalf("migrateRefs=%v 时不存在空间应 404，得到 %v", migrate, err)
		}
	}
}

// 级联删除后必须清理会话物理目录 sessions/<sid>（避免留垃圾数据）。
func TestWorkspaceDeleteCleansSessionDir(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	main, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	sessStore, err := store.OpenSessionStore(store.SessionStoreOptions{DataDir: dir, MaxOpen: 8})
	if err != nil {
		t.Fatalf("打开会话库失败: %v", err)
	}
	t.Cleanup(func() { _ = sessStore.Close() })

	svc := NewWorkspaceService(main, nil).WithSessionStore(sessStore)
	seedWsWithSession(t, ctx, main, "ws-dir")

	// 造出会话物理目录（Acquire 会惰性创建 sessions/<sid>）。
	if _, err := sessStore.Acquire(ctx, "s-ws-dir"); err != nil {
		t.Fatalf("创建会话目录失败: %v", err)
	}
	sessDir := sessStore.SessionDir("s-ws-dir")
	if _, err := os.Stat(sessDir); err != nil {
		t.Fatalf("会话目录应已存在: %v", err)
	}

	if err := svc.Delete(ctx, "ws-dir", true); err != nil {
		t.Fatalf("级联删除失败: %v", err)
	}
	// 目录必须被清理（孤儿数据不该留在磁盘上）。
	if _, err := os.Stat(sessDir); !os.IsNotExist(err) {
		t.Fatalf("会话物理目录应已被清理，stat 得到 err=%v", err)
	}
}
