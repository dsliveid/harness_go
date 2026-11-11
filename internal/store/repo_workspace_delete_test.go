package store

import (
	"context"
	"errors"
	"testing"

	"harness-go/internal/model"
)

// ============ 工作空间删除：严格模式 vs 迁移引用模式 ============
//
// 背景（用户报告的真实 bug）：Web 管理端点「移除」工作空间后刷新界面它又出现了。
// 根因有两层：
//  1. 前端只改了本地状态、从未调 DELETE（前端侧修复）；
//  2. 服务端 Delete 是严格模式：只要有会话/项目引用就 ErrWorkspaceInUse，
//     而实际场景里空间下必然挂着会话/项目 —— 即使调了 DELETE 也永远 409 删不掉。
//
// 本文件锁死第 2 层的契约：DeleteMigratingRefs 必须在同一事务内把引用迁回
// 内置 ws-server 后物理删除，且绝不误删会话/项目本身。

// deleteEnv 构造一个带主库的测试环境。
func deleteEnv(t *testing.T) (context.Context, *MainStore) {
	t.Helper()
	ctx := context.Background()
	main, err := OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	return ctx, main
}

// seedWsWithRefs 造一个非内置空间 + 一个项目 + 一个会话，全部归属该空间。
func seedWsWithRefs(t *testing.T, ctx context.Context, main *MainStore, wsID string) {
	t.Helper()
	w := &model.Workspace{ID: wsID, Name: wsID, Kind: model.WorkspaceKindDesktop}
	if err := main.Workspaces().Create(ctx, w); err != nil {
		t.Fatalf("创建空间失败: %v", err)
	}
	p := &model.Project{ID: "p-" + wsID, Name: "项目", WorkspaceID: wsID}
	if err := main.Projects().Create(ctx, p); err != nil {
		t.Fatalf("创建项目失败: %v", err)
	}
	s := &model.Session{ID: "s-" + wsID, Title: "会话", WorkspaceID: wsID}
	if err := main.Sessions().Create(ctx, s); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
}

// countWsRefs 统计某空间下的会话数与项目数。
func countWsRefs(t *testing.T, ctx context.Context, main *MainStore, wsID string) (sessions, projects int) {
	t.Helper()
	if err := main.conn().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE workspace_id=?`, wsID).Scan(&sessions); err != nil {
		t.Fatalf("统计会话引用失败: %v", err)
	}
	if err := main.conn().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM projects WHERE workspace_id=?`, wsID).Scan(&projects); err != nil {
		t.Fatalf("统计项目引用失败: %v", err)
	}
	return sessions, projects
}

// 严格模式：有引用就拒绝（旧契约不可回退，否则会有人静默级联丢执行面）。
func TestWorkspaceDeleteStrictRejectsWhenReferenced(t *testing.T) {
	ctx, main := deleteEnv(t)
	seedWsWithRefs(t, ctx, main, "ws-strict")

	err := main.Workspaces().Delete(ctx, "ws-strict")
	if !errors.Is(err, ErrWorkspaceInUse) {
		t.Fatalf("严格模式应返回 ErrWorkspaceInUse，得到 %v", err)
	}
	// 空间必须仍然存在（拒绝删除不能产生半删状态）
	if _, err := main.Workspaces().Get(ctx, "ws-strict"); err != nil {
		t.Fatalf("严格模式被拒后空间应仍存在，却查不到: %v", err)
	}
}

// 级联模式（用户「确认移除」的真实语义）：会话/项目一并删除，空间行物理消失。
func TestWorkspaceDeleteMigratingRefs(t *testing.T) {
	ctx, main := deleteEnv(t)
	seedWsWithRefs(t, ctx, main, "ws-mig")

	// 给项目挂一条审批规则，验证级联会把规则一并清掉。
	if _, err := main.Projects().AddRule(ctx, "p-ws-mig", "write_file", "*"); err != nil {
		t.Fatalf("添加项目规则失败: %v", err)
	}

	deleted, err := main.Workspaces().DeleteMigratingRefs(ctx, "ws-mig")
	if err != nil {
		t.Fatalf("级联删除应成功，得到 %v", err)
	}
	// ① 必须回报被软删的会话 ID（上层据此清理物理目录）。
	if len(deleted) != 1 || deleted[0] != "s-ws-mig" {
		t.Fatalf("应回报被删会话 ID [s-ws-mig]，得到 %v", deleted)
	}
	// ② 空间行必须真的没了 —— 这是「刷新又出现」bug 的根，必须物理删除。
	if _, err := main.Workspaces().Get(ctx, "ws-mig"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("空间应已物理删除（期望 ErrNotFound），得到 %v", err)
	}
	// ③ 会话与项目必须被软删：Get 一律过滤已删 → 应报 ErrNotFound。
	if _, err := main.Sessions().Get(ctx, "s-ws-mig"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("会话应已删除（期望 ErrNotFound），得到 %v", err)
	}
	if _, err := main.Projects().Get(ctx, "p-ws-mig"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("项目应已删除（期望 ErrNotFound），得到 %v", err)
	}
	// ④ 该项目的审批规则必须被清掉（否则残留规则会持续影响同名工具裁决）。
	rules, err := main.Projects().ListRules(ctx, "p-ws-mig")
	if err != nil {
		t.Fatalf("列规则失败: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("项目规则应被清空，仍剩 %d 条", len(rules))
	}
	// ⑤ 🚨 安全铁律：绝不能把引用迁到 ws-server —— 那会让 readonly/离线会话
	//    获得 ToolPolicyFull（可写文件 + 执行命令），属静默提权（违反 D8）。
	if s, p := countWsRefs(t, ctx, main, BuiltinWorkspaceServerID); s != 0 || p != 0 {
		t.Fatalf("引用不得迁入 %s（会提权到 full），得到会话 %d / 项目 %d",
			BuiltinWorkspaceServerID, s, p)
	}
}

// 级联模式也不许删内置空间（它是历史回填兜底，删掉会让引用悬空）。
func TestWorkspaceDeleteMigratingRefsRejectsBuiltin(t *testing.T) {
	ctx, main := deleteEnv(t)
	if _, err := main.Workspaces().DeleteMigratingRefs(ctx, BuiltinWorkspaceServerID); err == nil {
		t.Fatal("内置服务端空间不可删除，却返回了成功")
	}
	if _, err := main.Workspaces().Get(ctx, BuiltinWorkspaceServerID); err != nil {
		t.Fatalf("内置空间应仍存在，却查不到: %v", err)
	}
}

// 不存在的 ID：必须报 ErrNotFound（否则前端会收到假成功、卡片消失但库无变化）。
func TestWorkspaceDeleteMigratingRefsNotFound(t *testing.T) {
	ctx, main := deleteEnv(t)
	if _, err := main.Workspaces().DeleteMigratingRefs(ctx, "ws-ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的空间应返回 ErrNotFound，得到 %v", err)
	}
}
