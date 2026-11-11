package auth

import (
	"context"
	"testing"
	"time"

	"harness-go/internal/model"
	"harness-go/internal/store"
)

// newResetTestService 造一个带真实主库（临时目录）的 auth.Service。
//
// 用真库而非 mock：ResetPassword 的关键行为之一是「吊销该账号现存令牌」，
// 这必须靠真实 SQL 才验证得到（UPDATE ... WHERE user_id=? AND revoked_at IS NULL）。
func newResetTestService(t *testing.T) (*Service, *store.MainStore) {
	t.Helper()
	ctx := context.Background()
	main, err := store.OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	svc := NewService(Options{Main: main, PasswordParams: TestParams()})
	return svc, main
}

// seedUser 建一个账号并返回它（口令哈希用低开销参数）。
func seedUser(t *testing.T, ctx context.Context, main *store.MainStore, username, password string) *model.User {
	t.Helper()
	hash, err := HashPassword(password, TestParams())
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	u := &model.User{
		ID:           "u-" + username,
		Username:     username,
		PasswordHash: hash,
		DisplayName:  username,
		Role:         "owner",
	}
	if err := main.Users().Create(ctx, u); err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	return u
}

// ResetPassword 的三条主干行为：改掉口令、吊销旧会话、旧口令立即失效。
func TestResetPasswordRotatesAndRevokes(t *testing.T) {
	ctx := context.Background()
	svc, main := newResetTestService(t)
	seedUser(t, ctx, main, "admin", "old-password-1")

	// 先用旧口令登录拿一个会话令牌（模拟「忘记密码前已在其它设备登录」）。
	_, token, err := svc.Login(ctx, "admin", "old-password-1")
	if err != nil {
		t.Fatalf("旧口令应能登录: %v", err)
	}
	if _, err := svc.VerifyToken(ctx, token); err != nil {
		t.Fatalf("重置前令牌应有效: %v", err)
	}

	revoked, err := svc.ResetPassword(ctx, "admin", "new-password-2")
	if err != nil {
		t.Fatalf("重置口令失败: %v", err)
	}
	if revoked != 1 {
		t.Fatalf("应吊销 1 个令牌，实际 %d", revoked)
	}

	// ① 旧令牌必须立即失效：否则「重置口令」对已泄露的 Cookie 毫无作用。
	if _, err := svc.VerifyToken(ctx, token); err == nil {
		t.Fatal("重置后旧会话令牌必须失效")
	}
	// ② 旧口令必须登不进去。
	if _, _, err := svc.Login(ctx, "admin", "old-password-1"); err == nil {
		t.Fatal("重置后旧口令不应能登录")
	}
	// ③ 新口令必须能登录。
	if _, _, err := svc.Login(ctx, "admin", "new-password-2"); err != nil {
		t.Fatalf("新口令应能登录: %v", err)
	}
}

// 幂等性：没有令牌可吊销时不报错，返回 0。
func TestResetPasswordIdempotentWithoutTokens(t *testing.T) {
	ctx := context.Background()
	svc, main := newResetTestService(t)
	seedUser(t, ctx, main, "admin", "old-password-1")

	revoked, err := svc.ResetPassword(ctx, "admin", "new-password-2")
	if err != nil {
		t.Fatalf("无令牌时重置不应报错: %v", err)
	}
	if revoked != 0 {
		t.Fatalf("无令牌时应返回 0，实际 %d", revoked)
	}
}

// 账号不存在必须报错（防「重置成功但实际没改」的静默失败）。
func TestResetPasswordUnknownUserFails(t *testing.T) {
	svc, _ := newResetTestService(t)
	if _, err := svc.ResetPassword(context.Background(), "nobody", "new-password-2"); err == nil {
		t.Fatal("账号不存在应报错")
	}
}

// 弱口令门槛与 ChangePassword 同口径（8 字符），且必须在写库前就拒绝。
func TestResetPasswordRejectsShortPassword(t *testing.T) {
	ctx := context.Background()
	svc, main := newResetTestService(t)
	seedUser(t, ctx, main, "admin", "old-password-1")

	if _, err := svc.ResetPassword(ctx, "admin", "short7c"); err == nil {
		t.Fatal("少于 8 字符应报错")
	}
	// 关键：被拒后原口令必须仍然可用（不能出现「校验失败但已写入」的半成品状态）。
	if _, _, err := svc.Login(ctx, "admin", "old-password-1"); err != nil {
		t.Fatalf("弱口令被拒后原口令应仍然有效: %v", err)
	}
}

// 空用户名直接拒绝（避免 TrimSpace 后按空串去查库）。
func TestResetPasswordRejectsEmptyUsername(t *testing.T) {
	svc, _ := newResetTestService(t)
	if _, err := svc.ResetPassword(context.Background(), "   ", "new-password-2"); err == nil {
		t.Fatal("空用户名应报错")
	}
}

// 用户名两侧空白应被容忍（CLI 传参/脚本粘贴常见）。
func TestResetPasswordTrimsUsername(t *testing.T) {
	ctx := context.Background()
	svc, main := newResetTestService(t)
	seedUser(t, ctx, main, "admin", "old-password-1")

	if _, err := svc.ResetPassword(ctx, "  admin  ", "new-password-2"); err != nil {
		t.Fatalf("用户名两侧空白应被容忍: %v", err)
	}
	if _, _, err := svc.Login(ctx, "admin", "new-password-2"); err != nil {
		t.Fatalf("新口令应能登录: %v", err)
	}
}

// RevokeByUser 只动目标账号，绝不误伤他人令牌（越权吊销是严重事故）。
func TestResetPasswordOnlyRevokesTargetUser(t *testing.T) {
	ctx := context.Background()
	svc, main := newResetTestService(t)
	seedUser(t, ctx, main, "admin", "old-password-1")
	seedUser(t, ctx, main, "alice", "alice-password-1")

	_, adminToken, err := svc.Login(ctx, "admin", "old-password-1")
	if err != nil {
		t.Fatalf("admin 登录失败: %v", err)
	}
	_, aliceToken, err := svc.Login(ctx, "alice", "alice-password-1")
	if err != nil {
		t.Fatalf("alice 登录失败: %v", err)
	}

	if _, err := svc.ResetPassword(ctx, "admin", "new-password-2"); err != nil {
		t.Fatalf("重置失败: %v", err)
	}
	if _, err := svc.VerifyToken(ctx, adminToken); err == nil {
		t.Fatal("admin 旧令牌应失效")
	}
	if _, err := svc.VerifyToken(ctx, aliceToken); err != nil {
		t.Fatalf("alice 令牌不应被误伤: %v", err)
	}
}

// 过期令牌同样被标记吊销（不能因为「已经过期」就跳过，否则语义不统一）。
func TestRevokeByUserMarksEvenExpiredTokens(t *testing.T) {
	ctx := context.Background()
	_, main := newResetTestService(t)
	u := seedUser(t, ctx, main, "admin", "old-password-1")

	past := time.Now().Add(-time.Hour).UnixMilli()
	plain := "hgt_deadbeefdeadbeefdeadbeef"
	tk := &model.Token{
		ID:        "tk-1",
		UserID:    u.ID,
		Name:      "old",
		Kind:      model.TokenKindWebSession,
		TokenHash: HashToken(plain),
		Scope:     "write",
		ExpiresAt: &past,
	}
	if err := main.Tokens().Create(ctx, tk); err != nil {
		t.Fatalf("写入令牌失败: %v", err)
	}

	n, err := main.Tokens().RevokeByUser(ctx, u.ID, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("吊销失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应吊销 1 条，实际 %d", n)
	}
	// 再调一次必须返回 0（已吊销的不重复计数）。
	if n, err := main.Tokens().RevokeByUser(ctx, u.ID, time.Now().UnixMilli()); err != nil || n != 0 {
		t.Fatalf("重复吊销应为 0 且不报错: n=%d err=%v", n, err)
	}
}
