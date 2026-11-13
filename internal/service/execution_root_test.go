package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"harness-go/internal/model"
	"harness-go/internal/store"
)

// ============ 会话执行根解析（2026-11 目录选择放开）============
//
// 这是"放开选目录"与"不许裸奔"之间的**唯一闸门**，因此每条分支都必须有断言：
//   - 无项目目录 / 纯对话 → 空根（回落进程根），不是错误；
//   - 越界 → 空根 + error（调用方据此回落并告警）；
//   - enforce=false → 跳过白名单，仅保留绝对/存在/是目录校验；
//   - 软链逃逸 → 拒绝（真实路径也必须落在白名单内）。

// newExecRootEnv 造主库 + 两个临时目录（白名单内 / 白名单外）。
func newExecRootEnv(t *testing.T) (main *store.MainStore, ctx context.Context, inside, outside string) {
	t.Helper()
	ctx = context.Background()
	main, err := store.OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })

	base := t.TempDir()
	inside = filepath.Join(base, "inside")
	outside = filepath.Join(t.TempDir(), "outside")
	for _, d := range []string{inside, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("建目录 %s 失败: %v", d, err)
		}
	}
	return main, ctx, inside, outside
}

// addSess 插入一个带 project_dir 的会话。
func addSess(t *testing.T, main *store.MainStore, ctx context.Context, id, projectDir string) {
	t.Helper()
	if err := main.Sessions().Create(ctx, &model.Session{
		ID: id, Title: id, ProjectDir: projectDir,
		WorkspaceID: store.BuiltinWorkspaceServerID,
	}); err != nil {
		t.Fatalf("插入会话失败: %v", err)
	}
}

// ① 白名单内的项目目录 → 原样返回（且已 Clean）。
func TestExecutionRootAcceptsInsideRoot(t *testing.T) {
	main, ctx, inside, _ := newExecRootEnv(t)
	addSess(t, main, ctx, "s-1", inside)

	r := NewExecutionRootResolver(main, []string{filepath.Dir(inside)}, true, nil)
	got, err := r.ResolveExecutionRoot(ctx, "s-1")
	if err != nil {
		t.Fatalf("白名单内目录不应报错: %v", err)
	}
	if filepath.Clean(got) != filepath.Clean(inside) {
		t.Fatalf("应返回白名单内目录 %q，得到 %q", inside, got)
	}
}

// ② 白名单外 → 返回 error（调用方必须回落进程根，绝不能放行）。
func TestExecutionRootRejectsOutsideRoot(t *testing.T) {
	main, ctx, inside, outside := newExecRootEnv(t)
	addSess(t, main, ctx, "s-2", outside)

	r := NewExecutionRootResolver(main, []string{inside}, true, nil)
	got, err := r.ResolveExecutionRoot(ctx, "s-2")
	if err == nil {
		t.Fatalf("白名单外目录必须报错，实际返回 %q", got)
	}
	if got != "" {
		t.Fatalf("越界时必须返回空根，实际 %q", got)
	}
}

// ③ 纯对话（无 project_dir）→ 空根且**不报错**（这是正常状态）。
func TestExecutionRootEmptyProjectDirIsNotError(t *testing.T) {
	main, ctx, inside, _ := newExecRootEnv(t)
	addSess(t, main, ctx, "s-3", "")

	r := NewExecutionRootResolver(main, []string{inside}, true, nil)
	got, err := r.ResolveExecutionRoot(ctx, "s-3")
	if err != nil {
		t.Fatalf("纯对话不应报错: %v", err)
	}
	if got != "" {
		t.Fatalf("纯对话应返回空根，得到 %q", got)
	}
}

// ④ 不存在的目录 → error（不允许把死路径当执行根）。
func TestExecutionRootRejectsMissingDir(t *testing.T) {
	main, ctx, inside, _ := newExecRootEnv(t)
	missing := filepath.Join(inside, "not-exist")
	addSess(t, main, ctx, "s-4", missing)

	r := NewExecutionRootResolver(main, []string{inside}, true, nil)
	if _, err := r.ResolveExecutionRoot(ctx, "s-4"); err == nil {
		t.Fatal("不存在的目录必须报错")
	}
}

// ⑤ 相对路径项目目录 → error（语义依赖进程 CWD，不可控）。
func TestExecutionRootRejectsRelativePath(t *testing.T) {
	main, ctx, inside, _ := newExecRootEnv(t)
	addSess(t, main, ctx, "s-5", "relative/dir")

	r := NewExecutionRootResolver(main, []string{inside}, true, nil)
	if _, err := r.ResolveExecutionRoot(ctx, "s-5"); err == nil {
		t.Fatal("相对路径必须报错")
	}
}

// ⑥ enforce=false → 白名单外的目录也被接受（显式降级，供单机自用）。
func TestExecutionRootEnforceDisabledAllowsOutside(t *testing.T) {
	main, ctx, inside, outside := newExecRootEnv(t)
	addSess(t, main, ctx, "s-6", outside)

	r := NewExecutionRootResolver(main, []string{inside}, false, nil)
	got, err := r.ResolveExecutionRoot(ctx, "s-6")
	if err != nil {
		t.Fatalf("enforce=false 时不应因越界报错: %v", err)
	}
	if filepath.Clean(got) != filepath.Clean(outside) {
		t.Fatalf("应接受白名单外目录 %q，得到 %q", outside, got)
	}
}

// ⑦ 会话不存在 → 空根且不报错（老数据/已删除会话，回落进程根）。
func TestExecutionRootUnknownSessionIsEmpty(t *testing.T) {
	main, ctx, inside, _ := newExecRootEnv(t)

	r := NewExecutionRootResolver(main, []string{inside}, true, nil)
	got, err := r.ResolveExecutionRoot(ctx, "sess-does-not-exist")
	if err != nil {
		t.Fatalf("未知会话不应报错: %v", err)
	}
	if got != "" {
		t.Fatalf("未知会话应返回空根，得到 %q", got)
	}
}

// ⑧ roots 为空（未开放任何根）→ 一律空根（失败即关闭，与改造前行为一致）。
func TestExecutionRootNoRootsIsClosedByDefault(t *testing.T) {
	main, ctx, inside, _ := newExecRootEnv(t)
	addSess(t, main, ctx, "s-8", inside)

	r := NewExecutionRootResolver(main, nil, true, nil)
	got, err := r.ResolveExecutionRoot(ctx, "s-8")
	if err != nil {
		t.Fatalf("未开放根不应报错: %v", err)
	}
	if got != "" {
		t.Fatalf("未开放任何根时应返回空根（回落进程根），得到 %q", got)
	}
}

// ⑨ nil 解析器 / nil 主库 → 空根，绝不 panic（部分部署形态的安全网）。
func TestExecutionRootNilSafety(t *testing.T) {
	var nilR *ExecutionRootResolver
	if _, err := nilR.ResolveExecutionRoot(context.Background(), "s-1"); err != nil {
		t.Fatalf("nil 解析器不应报错: %v", err)
	}
	r := NewExecutionRootResolver(nil, []string{"C:/x"}, true, nil)
	if _, err := r.ResolveExecutionRoot(context.Background(), "s-1"); err != nil {
		t.Fatalf("nil 主库不应报错: %v", err)
	}
}

// ⑩ Roots() 返回副本：外部修改不得污染内部白名单。
func TestExecutionRootRootsIsCopy(t *testing.T) {
	r := NewExecutionRootResolver(nil, []string{"/a", "/b"}, true, nil)
	got := r.Roots()
	if len(got) != 2 {
		t.Fatalf("应有 2 个根，得到 %d", len(got))
	}
	got[0] = "/hacked"
	if r.Roots()[0] == "/hacked" {
		t.Fatal("Roots() 必须返回副本")
	}
}
