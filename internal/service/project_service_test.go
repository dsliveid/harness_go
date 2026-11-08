package service

// 项目地基自检（阶段三）：SessionService.Create 的权限裁决链 + ProjectService 校验语义。
// 裁决优先级：显式 access_mode > 项目 default_access_mode（新建时快照）> repo 层 confirm 兜底。

import (
	"context"
	"errors"
	"testing"

	"harness-go/internal/apperr"
	"harness-go/internal/store"
)

func newProjectTestEnv(t *testing.T) (*ProjectService, *SessionService, *store.MainStore) {
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
	return NewProjectService(main), NewSessionService(main, sessions), main
}

// 裁决链：显式 full 优先于项目默认 confirm。
func TestSessionCreateAccessModeExplicitWins(t *testing.T) {
	ctx := context.Background()
	projSvc, sessSvc, main := newProjectTestEnv(t)
	p, err := projSvc.Create(ctx, "项目A", `D:\a`, "confirm")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessSvc.Create(ctx, CreateSessionInput{
		Title: "显式", ProjectID: p.ID, AccessMode: "full",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sess.AccessMode != "full" {
		t.Fatalf("显式权限应生效，得到 %q", sess.AccessMode)
	}
	back, err := main.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.ProjectID != p.ID || back.AccessMode != "full" {
		t.Fatalf("落库不一致: %+v", back)
	}
}

// 裁决链：无显式值时继承项目默认 full（快照），事后改项目默认不影响既有会话。
func TestSessionCreateInheritsProjectDefault(t *testing.T) {
	ctx := context.Background()
	projSvc, sessSvc, main := newProjectTestEnv(t)
	p, err := projSvc.Create(ctx, "项目B", `D:\b`, "full")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessSvc.Create(ctx, CreateSessionInput{Title: "继承", ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if sess.AccessMode != "full" {
		t.Fatalf("应继承项目默认 full，得到 %q", sess.AccessMode)
	}
	// 改项目默认值 → 既有会话不受影响（快照语义）
	if err := projSvc.SetDefaultAccessMode(ctx, p.ID, "confirm"); err != nil {
		t.Fatal(err)
	}
	back, err := main.Sessions().Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.AccessMode != "full" {
		t.Fatalf("既有会话应保持快照值 full，得到 %q", back.AccessMode)
	}
	// 新会话才继承新默认值 confirm
	sess2, err := sessSvc.Create(ctx, CreateSessionInput{Title: "继承2", ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if sess2.AccessMode != "confirm" {
		t.Fatalf("新会话应继承新默认 confirm，得到 %q", sess2.AccessMode)
	}
}

// 裁决链：无显式值、无项目 → repo 层 confirm 兜底。
func TestSessionCreateFallsBackToConfirm(t *testing.T) {
	ctx := context.Background()
	_, sessSvc, _ := newProjectTestEnv(t)
	sess, err := sessSvc.Create(ctx, CreateSessionInput{Title: "纯对话"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.AccessMode != "confirm" {
		t.Fatalf("兜底应为 confirm，得到 %q", sess.AccessMode)
	}
	if sess.ProjectID != "" {
		t.Fatalf("纯对话 project_id 应为空，得到 %q", sess.ProjectID)
	}
}

// 裁决链：非法 access_mode / 不存在的项目 一律 400。
func TestSessionCreateRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	_, sessSvc, _ := newProjectTestEnv(t)
	if _, err := sessSvc.Create(ctx, CreateSessionInput{AccessMode: "yolo"}); !isInvalidArg(t, err) {
		t.Fatalf("非法 access_mode 应 invalid_argument，得到 %v", err)
	}
	if _, err := sessSvc.Create(ctx, CreateSessionInput{ProjectID: "proj-不存在"}); !isInvalidArg(t, err) {
		t.Fatalf("不存在的项目应 invalid_argument，得到 %v", err)
	}
}

// SetProject：空串解绑；非空先校验项目存在。
func TestSessionSetProject(t *testing.T) {
	ctx := context.Background()
	projSvc, sessSvc, main := newProjectTestEnv(t)
	p, err := projSvc.Create(ctx, "项目C", `D:\c`, "")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessSvc.Create(ctx, CreateSessionInput{Title: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sessSvc.SetProject(ctx, sess.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if back, _ := main.Sessions().Get(ctx, sess.ID); back.ProjectID != p.ID {
		t.Fatalf("SetProject 未生效: %q", back.ProjectID)
	}
	if err := sessSvc.SetProject(ctx, sess.ID, "proj-不存在"); !isInvalidArg(t, err) {
		t.Fatalf("绑定不存在项目应 invalid_argument，得到 %v", err)
	}
	if err := sessSvc.SetProject(ctx, sess.ID, ""); err != nil {
		t.Fatal(err)
	}
	if back, _ := main.Sessions().Get(ctx, sess.ID); back.ProjectID != "" {
		t.Fatalf("解绑后应为空: %q", back.ProjectID)
	}
}

// ProjectService 校验语义：name 回落 path basename、二者皆空 400、Update 空串=不修改。
func TestProjectServiceValidation(t *testing.T) {
	ctx := context.Background()
	projSvc, _, _ := newProjectTestEnv(t)
	if _, err := projSvc.Create(ctx, "", "", ""); !isInvalidArg(t, err) {
		t.Fatalf("name/path 皆空应 invalid_argument，得到 %v", err)
	}
	p, err := projSvc.Create(ctx, "", `D:\ws\my-proj`, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "my-proj" {
		t.Fatalf("name 应回落 basename，得到 %q", p.Name)
	}
	if p.DefaultAccessMode != "confirm" {
		t.Fatalf("空权限应回落 confirm，得到 %q", p.DefaultAccessMode)
	}
	if _, err := projSvc.Create(ctx, "x", "", "yolo"); !isInvalidArg(t, err) {
		t.Fatalf("非法 default_access_mode 应 invalid_argument，得到 %v", err)
	}
	// Update：name/权限空串表示不修改，pinned 按入参覆盖
	if err := projSvc.Update(ctx, p.ID, "", "", true); err != nil {
		t.Fatal(err)
	}
	got, err := projSvc.Get(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "my-proj" || got.DefaultAccessMode != "confirm" || !got.Pinned {
		t.Fatalf("Update 语义错误: %+v", got)
	}
	// 规则：tool_name 必填、重复添加幂等、删除后 404
	if _, err := projSvc.AddRule(ctx, p.ID, "  ", ""); !isInvalidArg(t, err) {
		t.Fatalf("空 tool_name 应 invalid_argument，得到 %v", err)
	}
	r1, err := projSvc.AddRule(ctx, p.ID, "fs_write", `D:\ws\**`)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := projSvc.AddRule(ctx, p.ID, "fs_write", `D:\ws\**`)
	if err != nil {
		t.Fatalf("重复添加应幂等: %v", err)
	}
	if r1.ID != r2.ID {
		t.Fatalf("幂等应返回同一规则: %s vs %s", r1.ID, r2.ID)
	}
	rules, err := projSvc.ListRules(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("规则数期望 1，得到 %d", len(rules))
	}
	if err := projSvc.DeleteRule(ctx, p.ID, r1.ID); err != nil {
		t.Fatal(err)
	}
	if err := projSvc.DeleteRule(ctx, p.ID, r1.ID); err == nil {
		t.Fatal("重复删除应返回 not_found")
	}
	// Delete：级联后 Get 应 404
	if err := projSvc.Delete(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := projSvc.Get(ctx, p.ID); err == nil {
		t.Fatal("删除后 Get 应 404")
	}
}

// isInvalidArg 断言错误码为 invalid_argument（400）。
func isInvalidArg(t *testing.T, err error) bool {
	t.Helper()
	if err == nil {
		return false
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Code == apperr.CodeInvalidArgument
}
