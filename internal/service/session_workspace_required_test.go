package service

import (
	"context"
	"testing"

	"harness-go/internal/model"
	"harness-go/internal/store"
)

// R1 工作空间必填：新建会话必须落一个非空 workspace_id。
//
// 为什么必须锁死：会话归属是能力天花板的唯一依据。若允许空 workspace_id 落库，
// 会话会走 resolveToolPolicy 的异常分支，且 R2 准入校验失去判定依据。
// 改造后「纯对话」不再是「没有工作空间」，而是「绑 ws-server 且没有项目」。

// newSessionCreateEnv 构造会话创建测试环境。
func newSessionCreateEnv(t *testing.T) (*SessionService, *store.MainStore) {
	t.Helper()
	main, err := store.OpenMain(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	sessions, err := store.OpenSessionStore(store.SessionStoreOptions{DataDir: t.TempDir(), MaxOpen: 4})
	if err != nil {
		t.Fatalf("打开会话库失败: %v", err)
	}
	t.Cleanup(func() { _ = sessions.Close() })
	return NewSessionService(main, sessions), main
}

// TestCreateSessionDefaultsToServerWorkspace 无项目无工作空间 → 缺省绑服务端内置空间。
func TestCreateSessionDefaultsToServerWorkspace(t *testing.T) {
	svc, _ := newSessionCreateEnv(t)
	sess, err := svc.Create(context.Background(), CreateSessionInput{Title: "纯对话"})
	if err != nil {
		t.Fatalf("纯对话会话创建失败: %v", err)
	}
	if sess.WorkspaceID != store.BuiltinWorkspaceServerID {
		t.Fatalf("纯对话会话应缺省绑 %q，得到 %q",
			store.BuiltinWorkspaceServerID, sess.WorkspaceID)
	}
	if sess.ProjectID != "" {
		t.Fatalf("纯对话会话不应有项目，得到 %q", sess.ProjectID)
	}
}

// TestCreateSessionPersistsWorkspaceID 缺省归属必须真正落库（不只是返回值）。
func TestCreateSessionPersistsWorkspaceID(t *testing.T) {
	svc, main := newSessionCreateEnv(t)
	ctx := context.Background()
	sess, err := svc.Create(ctx, CreateSessionInput{Title: "落库校验"})
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	back, err := main.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("回读会话失败: %v", err)
	}
	if back.WorkspaceID != store.BuiltinWorkspaceServerID {
		t.Fatalf("落库的 workspace_id 应为 %q，得到 %q",
			store.BuiltinWorkspaceServerID, back.WorkspaceID)
	}
}

// TestCreateSessionKeepsExplicitWorkspace 显式指定的工作空间不被缺省值覆盖。
func TestCreateSessionKeepsExplicitWorkspace(t *testing.T) {
	svc, main := newSessionCreateEnv(t)
	ctx := context.Background()

	ws := &model.Workspace{
		ID: "ws-explicit", Name: "显式空间", Kind: model.WorkspaceKindServer,
		CreatedAt: store.NowMillis(), UpdatedAt: store.NowMillis(),
	}
	if err := main.Workspaces().Create(ctx, ws); err != nil {
		t.Fatalf("插入工作空间失败: %v", err)
	}
	sess, err := svc.Create(ctx, CreateSessionInput{Title: "显式归属", WorkspaceID: "ws-explicit"})
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if sess.WorkspaceID != "ws-explicit" {
		t.Fatalf("显式 workspace_id 应被保留，得到 %q", sess.WorkspaceID)
	}
}

// TestCreateSessionRejectsUnknownWorkspace 引用不存在的工作空间必须 400（不留悬空引用）。
func TestCreateSessionRejectsUnknownWorkspace(t *testing.T) {
	svc, _ := newSessionCreateEnv(t)
	if _, err := svc.Create(context.Background(),
		CreateSessionInput{Title: "悬空", WorkspaceID: "no-such-ws"}); err == nil {
		t.Fatal("引用不存在的工作空间应报错，却创建成功了")
	}
}
