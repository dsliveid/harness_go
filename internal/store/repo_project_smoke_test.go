package store

// 项目仓储地基自检：0008 迁移可正常应用 + 项目 CRUD/规则幂等/级联删除 +
// 会话 project_id 三处列同步（sessionColumns / Create / scanSession）无错列。
// 归属：项目级权限治理阶段一~三（后端地基）。

import (
	"context"
	"errors"
	"testing"

	"harness-go/internal/model"
)

func TestTmpProjectRepoSmoke(t *testing.T) {
	ctx := context.Background()
	m, err := OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	repo := m.Projects()
	p := &model.Project{ID: "proj-1", Name: "演示", Path: `D:\demo`, DefaultAccessMode: "full"}
	if err := repo.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if p.CreatedAt == 0 || p.UpdatedAt == 0 {
		t.Fatal("时间戳未填充")
	}
	got, err := repo.Get(ctx, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultAccessMode != "full" || got.Name != "演示" {
		t.Fatalf("读取不一致: %+v", got)
	}
	// 空串回落 confirm
	p2 := &model.Project{ID: "proj-2", Name: "b"}
	if err := repo.Create(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if p2.DefaultAccessMode != "confirm" {
		t.Fatalf("默认权限回落失败: %s", p2.DefaultAccessMode)
	}
	// pinned 排序
	if err := repo.Update(ctx, "proj-2", "b2", "full", true); err != nil {
		t.Fatal(err)
	}
	list, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "proj-2" || !list[0].Pinned {
		t.Fatalf("列表排序/置顶异常: %+v", list)
	}
	// 规则幂等
	r1, err := repo.AddRule(ctx, "proj-1", "fs_write", "")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := repo.AddRule(ctx, "proj-1", "fs_write", "")
	if err != nil {
		t.Fatalf("重复添加应幂等: %v", err)
	}
	if r1.ID != r2.ID {
		t.Fatalf("幂等返回不同行: %s vs %s", r1.ID, r2.ID)
	}
	rules, err := repo.ListRules(ctx, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("规则数期望 1，得到 %d", len(rules))
	}
	// 会话 project_id 三处同步（写入+读回）
	sess := &model.Session{ID: "s1", Title: "t", ProjectID: "proj-1", AccessMode: "full"}
	if err := m.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	back, err := m.Sessions().Get(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if back.ProjectID != "proj-1" || back.AccessMode != "full" || back.ProjectDir != "" {
		t.Fatalf("会话列错位: %+v", back)
	}
	// SetProject
	if err := m.Sessions().SetProject(ctx, "s1", "proj-2"); err != nil {
		t.Fatal(err)
	}
	back, _ = m.Sessions().Get(ctx, "s1")
	if back.ProjectID != "proj-2" {
		t.Fatalf("SetProject 失败: %s", back.ProjectID)
	}
	// 级联删除
	if err := repo.DeleteProjectCascade(ctx, "proj-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Get(ctx, "proj-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("软删后应 ErrNotFound: %v", err)
	}
	if rules, _ := repo.ListRules(ctx, "proj-1"); len(rules) != 0 {
		t.Fatalf("规则未清理: %d", len(rules))
	}
	if err := repo.DeleteProjectCascade(ctx, "proj-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除应 ErrNotFound: %v", err)
	}
	// 会话解绑：s1 当前归属 proj-2，删 proj-2 后应归零
	if err := repo.DeleteProjectCascade(ctx, "proj-2"); err != nil {
		t.Fatal(err)
	}
	back, err = m.Sessions().Get(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if back.ProjectID != "" {
		t.Fatalf("级联后会话 project_id 应归零: %q", back.ProjectID)
	}
}
