package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"harness-go/internal/auth"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// seedRotateUser 在临时数据目录造好 main.db + admin 账号 + 一个旧 api 令牌。
func seedRotateUser(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()
	main, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	defer func() { _ = main.Close() }()

	hash, err := auth.HashPassword("old-password-1", auth.TestParams())
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	u := &model.User{
		ID: "u1", Username: "admin", PasswordHash: hash, DisplayName: "admin", Role: "owner",
	}
	if err := main.Users().Create(ctx, u); err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	svc := auth.NewService(auth.Options{Main: main, PasswordParams: auth.TestParams()})
	if _, _, err := svc.EnsureBootstrapToken(ctx); err != nil {
		t.Fatalf("造旧 api 令牌失败: %v", err)
	}
}

// 子命令分流必须精确匹配首参数（不能被普通参数值误触发）。
func TestIsRotateAPITokenCmd(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"-addr", ":5611"}, false},
		{[]string{"rotate-api-token"}, true},
		{[]string{"rotate-api-token", "-data", "x"}, true},
		// 出现在第二位不算（避免把参数值误判成子命令）
		{[]string{"-data", "rotate-api-token"}, false},
	}
	for _, c := range cases {
		if got := isRotateAPITokenCmd(c.args); got != c.want {
			t.Fatalf("isRotateAPITokenCmd(%v) = %v，期望 %v", c.args, got, c.want)
		}
	}
}

// 核心断言：轮转后——① 旧 api 令牌必须失效；② 新令牌必须能通过 VerifyToken；
// ③ 明文必须落进 0600 的 initial_token；④ **不得出现在任何日志输出里**（本 bug 的原始形态）。
func TestRunRotateAPITokenRevokesOldAndWritesFile(t *testing.T) {
	dir := t.TempDir()
	seedRotateUser(t, dir)

	// 先确认基线：库里此时有 1 个有效 api 令牌，轮转后应变成 1 个（旧的废、新的立）。
	ctx := context.Background()
	var out, errOut bytes.Buffer
	// -print 走显式通道取明文（默认只写文件，测试里需要拿到值做后续校验）。
	if err := runRotateAPIToken([]string{"-data", dir, "-print"}, &out, &errOut); err != nil {
		t.Fatalf("轮转失败: %v（stderr=%s）", err, errOut.String())
	}
	if !strings.Contains(out.String(), "已轮转 API 令牌") {
		t.Fatalf("应给出成功提示，实际输出：%s", out.String())
	}
	// 旧令牌 1 条应被作废（seed 里只造了 1 条 api）。
	if !strings.Contains(out.String(), "已作废 1 个旧 api 令牌") {
		t.Fatalf("应作废 1 个旧令牌，实际输出：%s", out.String())
	}

	// ③ 文件必须存在；POSIX 下权限须为 0600。
	//
	// ⚠️ Windows 差异：NTFS 只区分只读位，os.WriteFile 的 perm 参数被忽略，
	// 实测得到 0666。这是平台事实而非本代码缺陷，因此断言按 runtime.GOOS 分流——
	// 但要在文档/注释里明确告知 Windows 用户「该文件对本机其他账号可读」。
	path := filepath.Join(dir, initialTokenFile)
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("初始令牌文件应存在: %v", statErr)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("令牌文件权限应为 0600，实际 %o", perm)
		}
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("读取令牌文件失败: %v", readErr)
	}
	content := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(content, "Authorization: Bearer hgt_") {
		t.Fatalf("文件应写成可直接 -H @file 的形态，实际：%q", content)
	}
	newToken := strings.TrimPrefix(content, "Authorization: Bearer ")

	// ② 新令牌必须真的可用。
	main2, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatalf("重开主库失败: %v", err)
	}
	defer func() { _ = main2.Close() }()
	svc2 := auth.NewService(auth.Options{Main: main2, PasswordParams: auth.TestParams()})
	if _, err := svc2.VerifyToken(ctx, newToken); err != nil {
		t.Fatalf("新令牌必须能通过校验: %v", err)
	}

	// ① 旧的 api 令牌应全部处于吊销态（按 kind 整体吊销）。
	n, cErr := main2.Tokens().CountByKind(ctx, model.TokenKindAPI)
	if cErr != nil {
		t.Fatalf("统计令牌失败: %v", cErr)
	}
	if n != 1 {
		t.Fatalf("轮转后应只剩 1 个有效 api 令牌，实际 %d", n)
	}
}

// ④ 最关键的安全断言：persistBootstrapToken 的返回值/行为里不得把明文漏进日志文本。
//
// 这里直接对 runRotateAPIToken 的**默认（无 -print）**输出取证：stdout 只应给出文件路径，
// 不得出现 hgt_ 明文。这正是原始 bug 的判据（当年明文进了 app.log，等于进了一个可回读的面板）。
func TestRunRotateAPITokenDoesNotEchoTokenByDefault(t *testing.T) {
	dir := t.TempDir()
	seedRotateUser(t, dir)

	var out, errOut bytes.Buffer
	if err := runRotateAPIToken([]string{"-data", dir}, &out, &errOut); err != nil {
		t.Fatalf("轮转失败: %v", err)
	}
	if strings.Contains(out.String(), "hgt_") {
		t.Fatalf("默认输出不得包含令牌明文，实际：%s", out.String())
	}
	if !strings.Contains(out.String(), initialTokenFile) {
		t.Fatalf("应提示令牌落在哪个文件，实际：%s", out.String())
	}
}

// 缺 main.db 时必须明确报错并提示 -data，而不是静默新建空库（与 reset-password 同判据）。
func TestRunRotateAPITokenFailsOnMissingDB(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	err := runRotateAPIToken([]string{"-data", dir}, &out, &errOut)
	if err == nil {
		t.Fatal("缺少 main.db 时应报错")
	}
	if !strings.Contains(err.Error(), "main.db") {
		t.Fatalf("错误信息应指明 main.db，实际：%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "main.db")); statErr == nil {
		t.Fatal("不应在 data 目录创建空的 main.db")
	}
}
