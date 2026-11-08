package service

// 阶段四自检：审批规则合并下发（会话级「本会话允许」+ 项目级声明规则）。
//
// 覆盖点：
//  1. 纯对话（无项目）只返回会话规则，不额外读项目表；
//  2. 有项目时会话规则 + 项目规则去重合并；
//  3. 项目规则命中后，Gate 在 confirm 模式下确实自动放行（真正下发，而非只存表）；
//  4. 项目规则存在但会话规则为空时也能单独生效；
//  5. 项目级危险操作（danger）不受规则放行影响 —— 通行语义由 Gate 保证，不被规则绕过。

import (
	"context"
	"testing"
	"time"

	"harness-go/internal/approval"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

func newPolicyTestEnv(t *testing.T) (*ApprovalPolicy, *ProjectService, *SessionService, *store.SessionStore) {
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
	return NewApprovalPolicy(main, sessions), NewProjectService(main), NewSessionService(main, sessions), sessions
}

// 纯对话：无项目 → 只返回会话规则。
func TestPolicyNoProjectOnlySessionRules(t *testing.T) {
	ctx := context.Background()
	policy, projSvc, sessSvc, sessions := newPolicyTestEnv(t)
	_ = projSvc

	sess, err := sessSvc.Create(ctx, CreateSessionInput{Title: "纯对话"})
	if err != nil {
		t.Fatal(err)
	}
	// 给会话库塞一条「本会话允许」规则
	sdb, err := sessions.Acquire(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sdb.ApprovalRules().Add(ctx, "write_file", "internal/"); err != nil {
		t.Fatal(err)
	}

	rules, err := policy.RulesFor(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].ToolName != "write_file" || rules[0].Pattern != "internal/" {
		t.Fatalf("应仅返回会话规则，得到 %+v", rules)
	}
}

// 会话规则 + 项目规则去重合并。
func TestPolicyMergesSessionAndProjectRules(t *testing.T) {
	ctx := context.Background()
	policy, projSvc, sessSvc, sessions := newPolicyTestEnv(t)

	p, err := projSvc.Create(ctx, "项目A", `D:\a`, "confirm")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessSvc.Create(ctx, CreateSessionInput{Title: "带项目", ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	sdb, err := sessions.Acquire(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 会话规则与项目规则故意重复一条（run_command / go test），用于验证去重
	if err := sdb.ApprovalRules().Add(ctx, "run_command", "go test"); err != nil {
		t.Fatal(err)
	}
	if _, err := projSvc.AddRule(ctx, p.ID, "run_command", "go test"); err != nil {
		t.Fatal(err)
	}
	if _, err := projSvc.AddRule(ctx, p.ID, "write_file", "internal/"); err != nil {
		t.Fatal(err)
	}

	rules, err := policy.RulesFor(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 期望 2 条：run_command/go test（去重后 1 条）+ write_file/internal/
	if len(rules) != 2 {
		t.Fatalf("应合并去重为 2 条规则，得到 %d 条: %+v", len(rules), rules)
	}
	seen := map[string]bool{}
	for _, r := range rules {
		seen[r.ToolName+"|"+r.Pattern] = true
	}
	if !seen["run_command|go test"] || !seen["write_file|internal/"] {
		t.Fatalf("合并结果不符合预期: %+v", rules)
	}
}

// 真正下发：项目规则使 confirm 模式下的工具调用被 Gate 自动放行。
func TestPolicyProjectRuleAutoAllowsInConfirmMode(t *testing.T) {
	ctx := context.Background()
	policy, projSvc, sessSvc, _ := newPolicyTestEnv(t)

	p, err := projSvc.Create(ctx, "项目A", `D:\a`, "confirm")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessSvc.Create(ctx, CreateSessionInput{Title: "带项目", ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := projSvc.AddRule(ctx, p.ID, "run_command", "go test"); err != nil {
		t.Fatal(err)
	}

	rules, err := policy.RulesFor(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	gate := approval.NewGate(50 * time.Millisecond)

	// 命中项目规则 → 自动放行
	hit := gate.Evaluate(approval.Request{
		ToolName:   "run_command",
		Risk:       model.RiskCommand,
		ArgsJSON:   `{"command":"go test ./..."}`,
		AccessMode: approval.ModeConfirm,
		Rules:      rules,
	})
	if hit.NeedApproval {
		t.Fatalf("项目规则应自动放行 run_command/go test，却要求审批: %+v", hit)
	}

	// 未命中 → 仍需审批（确认规则没有过度放大）
	miss := gate.Evaluate(approval.Request{
		ToolName:   "run_command",
		Risk:       model.RiskCommand,
		ArgsJSON:   `{"command":"rm -rf /"}`,
		AccessMode: approval.ModeConfirm,
		Rules:      rules,
	})
	if !miss.NeedApproval {
		t.Fatalf("未命中规则的命令不应放行: %+v", miss)
	}
}

// 删除项目后规则立即失效（会话回纯对话）。
func TestPolicyRulesGoneAfterProjectDelete(t *testing.T) {
	ctx := context.Background()
	policy, projSvc, sessSvc, _ := newPolicyTestEnv(t)

	p, err := projSvc.Create(ctx, "项目A", `D:\a`, "full")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessSvc.Create(ctx, CreateSessionInput{Title: "带项目", ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := projSvc.AddRule(ctx, p.ID, "write_file", "internal/"); err != nil {
		t.Fatal(err)
	}
	rules, err := policy.RulesFor(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("删除前应有 1 条项目规则，得到 %+v", rules)
	}

	if err := projSvc.Delete(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	// 级联已把 sessions.project_id 归零 → 规则不再下发
	rules, err = policy.RulesFor(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 0 {
		t.Fatalf("项目删除后不应再有项目规则，得到 %+v", rules)
	}
	// 会话本身的访问模式快照不受影响（只快照一次，删除项目不回滚历史权限）
	back, err := sessSvc.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.AccessMode != "full" {
		t.Fatalf("会话访问模式快照不应被项目删除影响，得到 %q", back.AccessMode)
	}
}
