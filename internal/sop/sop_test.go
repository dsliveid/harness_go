package sop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestLoader 建一个临时 dataDir 的 Loader。
func newTestLoader(t *testing.T, budget int) *Loader {
	t.Helper()
	return NewLoader(t.TempDir(), budget)
}

// TestBuiltinNonEmpty 出厂内容必须存在且包含三条核心规则（防止 embed 路径写错）。
func TestBuiltinNonEmpty(t *testing.T) {
	b := Builtin()
	if strings.TrimSpace(b) == "" {
		t.Fatal("出厂 SOP 不应为空")
	}
	for _, want := range []string{"修改文件前先读取确认内容", "危险操作", "回答简洁"} {
		if !strings.Contains(b, want) {
			t.Fatalf("出厂 SOP 缺少规则片段 %q，实际内容：\n%s", want, b)
		}
	}
}

// TestLoadCoreFallsBackToBuiltin 无覆盖文件时回落内置（场景 1：无覆盖）。
func TestLoadCoreFallsBackToBuiltin(t *testing.T) {
	l := newTestLoader(t, 800)
	core := l.LoadCore()

	if core.Source != SourceBuiltin {
		t.Fatalf("来源应为 builtin，实际 %s", core.Source)
	}
	if core.Content != Builtin() {
		t.Fatal("内容应等于出厂原文")
	}
	if core.Overridden {
		t.Fatal("Overridden 应为 false")
	}
	if core.BuiltinContent == "" {
		t.Fatal("BuiltinContent 必须始终可用（前端内置页签依赖它）")
	}
	if core.Tokens <= 0 {
		t.Fatalf("token 估算应 > 0，实际 %d", core.Tokens)
	}
}

// TestLoadCoreNeverSeeds 铁律断言：只读操作绝不得把出厂内容写到磁盘。
func TestLoadCoreNeverSeeds(t *testing.T) {
	l := newTestLoader(t, 800)
	for i := 0; i < 3; i++ {
		l.LoadCore()
	}
	if _, err := os.Stat(l.RuntimePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadCore 绝不应播种覆盖层，但文件已存在: %s", l.RuntimePath())
	}
	// dataDir/sop 目录本身也不应被创建。
	if _, err := os.Stat(filepath.Dir(l.RuntimePath())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadCore 不应创建 data/sop 目录")
	}
}

// TestSaveCoreOverridesBuiltin 保存后转为自定义（场景 2：有覆盖）。
func TestSaveCoreOverridesBuiltin(t *testing.T) {
	l := newTestLoader(t, 800)
	custom := "你是定制助手。\n规则：先思考再动手。"

	core, err := l.SaveCore(custom)
	if err != nil {
		t.Fatalf("SaveCore 失败: %v", err)
	}
	if core.Source != SourceCustom {
		t.Fatalf("来源应为 custom，实际 %s", core.Source)
	}
	if !core.Overridden {
		t.Fatal("Overridden 应为 true")
	}
	if core.Content != custom {
		t.Fatalf("内容不一致：%q", core.Content)
	}
	// 再读一次（等价于「下一轮对话」）必须仍是自定义内容。
	if got := l.LoadCore(); got.Content != custom || got.Source != SourceCustom {
		t.Fatalf("重新读取应仍为自定义，实际 source=%s content=%q", got.Source, got.Content)
	}
}

// TestResetCoreRestoresBuiltin 删除覆盖层即完整还原（场景 3：删除回落）。
func TestResetCoreRestoresBuiltin(t *testing.T) {
	l := newTestLoader(t, 800)
	if _, err := l.SaveCore("临时定制内容"); err != nil {
		t.Fatalf("SaveCore 失败: %v", err)
	}
	if err := l.ResetCore(); err != nil {
		t.Fatalf("ResetCore 失败: %v", err)
	}

	core := l.LoadCore()
	if core.Source != SourceBuiltin {
		t.Fatalf("还原后来源应为 builtin，实际 %s", core.Source)
	}
	if core.Content != Builtin() {
		t.Fatal("还原后内容应与出厂逐字一致")
	}
	if _, err := os.Stat(l.RuntimePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("还原后覆盖文件应已删除")
	}
}

// TestResetCoreIdempotent 重复还原不报错。
func TestResetCoreIdempotent(t *testing.T) {
	l := newTestLoader(t, 800)
	for i := 0; i < 2; i++ {
		if err := l.ResetCore(); err != nil {
			t.Fatalf("第 %d 次 ResetCore 不应报错: %v", i+1, err)
		}
	}
}

// TestSaveCoreBudgetExceeded 超预算显式拒绝，且磁盘不被修改（场景 4）。
func TestSaveCoreBudgetExceeded(t *testing.T) {
	l := newTestLoader(t, 10)
	if _, err := l.SaveCore("原本的合法内容"); err != nil {
		t.Fatalf("首次保存应成功: %v", err)
	}
	before, err := os.ReadFile(l.RuntimePath())
	if err != nil {
		t.Fatalf("读取原文件失败: %v", err)
	}

	long := strings.Repeat("这是一段很长很长的规则说明。", 50)
	_, sErr := l.SaveCore(long)
	if !errors.Is(sErr, ErrBudgetExceeded) {
		t.Fatalf("应返回 ErrBudgetExceeded，实际: %v", sErr)
	}

	after, err := os.ReadFile(l.RuntimePath())
	if err != nil {
		t.Fatalf("超预算后原文件应仍然存在: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("超预算被拒时，绝不允许修改磁盘文件（拒绝不截断）")
	}
	if got := l.LoadCore().Content; got != "原本的合法内容" {
		t.Fatalf("生效内容应保持原值，实际 %q", got)
	}
}

// TestSaveCoreBudgetZeroUnlimited budget=0 表示不限。
func TestSaveCoreBudgetZeroUnlimited(t *testing.T) {
	l := newTestLoader(t, 0)
	long := strings.Repeat("很长的规则。", 200)
	if _, err := l.SaveCore(long); err != nil {
		t.Fatalf("budget=0 不应限制长度: %v", err)
	}
}

// TestSaveCoreRejectsEmpty 空内容拒绝（引导用户改用 ResetCore）。
func TestSaveCoreRejectsEmpty(t *testing.T) {
	l := newTestLoader(t, 800)
	for _, s := range []string{"", "   ", "\n\t "} {
		if _, err := l.SaveCore(s); err == nil {
			t.Fatalf("空内容 %q 应被拒绝", s)
		}
	}
}

// TestEmptyFileTreatedAsNotOverridden 空文件视同未覆盖（场景 5：空文件容忍）。
func TestEmptyFileTreatedAsNotOverridden(t *testing.T) {
	l := newTestLoader(t, 800)
	if err := os.MkdirAll(filepath.Dir(l.RuntimePath()), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(l.RuntimePath(), []byte("   \n\t\n"), 0o600); err != nil {
		t.Fatalf("写空文件失败: %v", err)
	}
	core := l.LoadCore()
	if core.Source != SourceBuiltin {
		t.Fatalf("空文件应回落 builtin，实际 %s", core.Source)
	}
	if core.Overridden {
		t.Fatal("空文件不应标记为已覆盖")
	}
}

// TestNoDataDirDegrades dataDir 为空时退化为仅内置，读写返回 ErrNoDataDir。
func TestNoDataDirDegrades(t *testing.T) {
	l := NewLoader("", 800)
	core := l.LoadCore()
	if core.Source != SourceBuiltin || core.Content != Builtin() {
		t.Fatal("未配置 dataDir 时应回落内置")
	}
	if l.RuntimePath() != "" {
		t.Fatal("未配置 dataDir 时 RuntimePath 应为空")
	}
	if _, err := l.SaveCore("内容"); !errors.Is(err, ErrNoDataDir) {
		t.Fatalf("SaveCore 应返回 ErrNoDataDir，实际 %v", err)
	}
	if err := l.ResetCore(); !errors.Is(err, ErrNoDataDir) {
		t.Fatalf("ResetCore 应返回 ErrNoDataDir，实际 %v", err)
	}
}

// TestBudgetExposedToCore 预算须回传给前端展示。
func TestBudgetExposedToCore(t *testing.T) {
	if got := newTestLoader(t, 800).LoadCore().Budget; got != 800 {
		t.Fatalf("Budget 应为 800，实际 %d", got)
	}
}

// ---------- 项目级条目 ----------

// TestLoadEntries 读取 .harness/sop 下条目，按文件名排序、跳过目录与非 md。
func TestLoadEntries(t *testing.T) {
	ws := t.TempDir()
	dir := EntriesPath(ws)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	files := map[string]string{
		"b-rule.md": "# B\n内容B",
		"a-rule.md": "# A\n内容A",
		"note.txt":  "非 md 应忽略",
		"empty.md":  "  \n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("写 %s 失败: %v", name, err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("建子目录失败: %v", err)
	}

	got := LoadEntries(ws)
	if len(got) != 2 {
		t.Fatalf("应读到 2 条（a-rule/b-rule），实际 %d: %+v", len(got), got)
	}
	if got[0].ID != "a-rule" || got[1].ID != "b-rule" {
		t.Fatalf("应按文件名升序，实际 %s, %s", got[0].ID, got[1].ID)
	}
	for _, e := range got {
		if e.Body == "" || e.Tokens <= 0 {
			t.Fatalf("条目正文与 token 应有效: %+v", e)
		}
	}
}

// TestLoadEntriesNoWorkspace 未绑定工作区 / 目录不存在均返回 nil，不 panic。
func TestLoadEntriesNoWorkspace(t *testing.T) {
	if got := LoadEntries(""); got != nil {
		t.Fatalf("空 wsPath 应返回 nil，实际 %+v", got)
	}
	if got := LoadEntries(filepath.Join(t.TempDir(), "not-exist")); got != nil {
		t.Fatalf("目录不存在应返回 nil，实际 %+v", got)
	}
}

// TestValidateEntry 条数上限与非空校验。
func TestValidateEntry(t *testing.T) {
	full := []string{"a", "b", "c", "d", "e", "f"}
	if err := ValidateEntry("g", "正文", full, false); err == nil {
		t.Fatal("第 7 条新增应被拒绝")
	}
	if err := ValidateEntry("a", "正文", full, true); err != nil {
		t.Fatalf("更新已存在条目不应受条数限制: %v", err)
	}
	if err := ValidateEntry("g", "正文", []string{"a"}, false); err != nil {
		t.Fatalf("未满上限应通过: %v", err)
	}
	if err := ValidateEntry("", "正文", nil, false); err == nil {
		t.Fatal("空 ID 应被拒绝")
	}
	if err := ValidateEntry("x", "   ", nil, false); err == nil {
		t.Fatal("空正文应被拒绝")
	}
}

// TestSlugify 标题转 ID：保留 CJK/字母数字，其余折叠为 -。
func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"方案先行":         "方案先行",
		"Plan First":    "plan-first",
		"a/b\\c":        "a-b-c",
		"  --Edge--  ": "edge",
		"":              "",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Fatalf("Slugify(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestEstimateTokens 数量级正确性（CJK ≈1/字，ASCII ≈1/4 字符）。
func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens(""); got != 0 {
		t.Fatalf("空串应为 0，实际 %d", got)
	}
	if got := EstimateTokens("中文四个字"); got != 5 {
		t.Fatalf("CJK 应按字计数，实际 %d", got)
	}
	if got := EstimateTokens("abcdefgh"); got != 2 {
		t.Fatalf("8 个 ASCII 应约 2 token，实际 %d", got)
	}
}
