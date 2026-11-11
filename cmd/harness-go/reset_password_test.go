package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness-go/internal/auth"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// seedResetCLIUser 在临时数据目录里造好 main.db 与一个 admin 账号。
func seedResetCLIUser(t *testing.T, dir, username, password string) {
	t.Helper()
	ctx := context.Background()
	main, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	defer func() { _ = main.Close() }()

	hash, err := auth.HashPassword(password, auth.TestParams())
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	u := &model.User{
		ID:           "u1",
		Username:     username,
		PasswordHash: hash,
		DisplayName:  username,
		Role:         "owner",
	}
	if err := main.Users().Create(ctx, u); err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
}

// 子命令分流必须精确匹配首参数，不能吞掉普通的服务端启动参数。
func TestIsResetPasswordCmd(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"-addr", ":5611"}, false},
		{[]string{"reset-password"}, true},
		{[]string{"reset-password", "-user", "admin"}, true},
		// 出现在第二位不算子命令（避免误判普通参数值）
		{[]string{"-data", "reset-password"}, false},
	}
	for _, c := range cases {
		if got := isResetPasswordCmd(c.args); got != c.want {
			t.Fatalf("isResetPasswordCmd(%v) = %v，期望 %v", c.args, got, c.want)
		}
	}
}

// 端到端：随机口令 → 打印出来的口令必须真的能登录（防止「打印的不是实际写入的」）。
func TestRunResetPasswordGeneratesWorkingPassword(t *testing.T) {
	dir := t.TempDir()
	seedResetCLIUser(t, dir, "admin", "old-password-1")

	var out, errOut bytes.Buffer
	if err := runResetPassword([]string{"-data", dir}, &out, &errOut); err != nil {
		t.Fatalf("重置失败: %v（stderr=%s）", err, errOut.String())
	}

	pwd := extractGeneratedPassword(t, out.String())

	ctx := context.Background()
	main, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	defer func() { _ = main.Close() }()
	svc := auth.NewService(auth.Options{Main: main, PasswordParams: auth.TestParams()})

	if _, _, err := svc.Login(ctx, "admin", pwd); err != nil {
		t.Fatalf("命令行打印出的新口令必须能登录: %v", err)
	}
	if _, _, err := svc.Login(ctx, "admin", "old-password-1"); err == nil {
		t.Fatal("旧口令应已失效")
	}
}

// 显式传 -password 时不回显口令本身（避免口令被复制进日志/终端记录）。
func TestRunResetPasswordExplicitPasswordNotEchoed(t *testing.T) {
	dir := t.TempDir()
	seedResetCLIUser(t, dir, "admin", "old-password-1")

	var out, errOut bytes.Buffer
	if err := runResetPassword([]string{"-data", dir, "-password", "brand-new-pass-9"}, &out, &errOut); err != nil {
		t.Fatalf("重置失败: %v", err)
	}
	if strings.Contains(out.String(), "brand-new-pass-9") {
		t.Fatalf("显式口令不应回显，实际输出：%s", out.String())
	}
	if !strings.Contains(out.String(), "已重置账号") {
		t.Fatalf("应给出成功提示，实际输出：%s", out.String())
	}
}

// 数据目录里没有 main.db 时，必须明确报错并提示用 -data 指定，而不是静默新建空库。
func TestRunResetPasswordFailsOnMissingDB(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	err := runResetPassword([]string{"-data", dir}, &out, &errOut)
	if err == nil {
		t.Fatal("缺少 main.db 时应报错")
	}
	if !strings.Contains(err.Error(), "main.db") {
		t.Fatalf("错误信息应指明 main.db，实际：%v", err)
	}
	// 关键：绝不能顺手建出一个空 main.db（否则会报「账号不存在」，误导排查方向）。
	if _, statErr := os.Stat(filepath.Join(dir, "main.db")); statErr == nil {
		t.Fatal("不应在 data 目录创建空的 main.db")
	}
}

// 账号不存在时报错，且错误里带账号名便于排查。
func TestRunResetPasswordUnknownUser(t *testing.T) {
	dir := t.TempDir()
	seedResetCLIUser(t, dir, "admin", "old-password-1")

	var out, errOut bytes.Buffer
	err := runResetPassword([]string{"-data", dir, "-user", "nobody"}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "nobody") {
		t.Fatalf("应报账号不存在且带账号名，实际：%v", err)
	}
}

// extractGeneratedPassword 从命令行输出中取出随机口令。
//
// 用显式解析而非正则全量匹配，是为了让「输出格式被改动」直接以失败暴露，
// 而不是悄悄取到一串错的东西再测试出一个假通过。
func extractGeneratedPassword(t *testing.T, output string) string {
	t.Helper()
	const marker = "请立即保存）："
	for _, line := range strings.Split(output, "\n") {
		if idx := strings.Index(line, marker); idx >= 0 {
			pwd := strings.TrimSpace(line[idx+len(marker):])
			if pwd == "" {
				t.Fatalf("解析到空口令，原始输出：%s", output)
			}
			return pwd
		}
	}
	t.Fatalf("输出中未找到随机口令行，原始输出：%s", output)
	return ""
}
