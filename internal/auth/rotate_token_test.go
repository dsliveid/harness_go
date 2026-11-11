package auth

import (
	"context"
	"testing"
	"time"

	"harness-go/internal/model"
)

// 轮转的核心契约：旧 api 令牌全部失效、新令牌可用、**web_session 不被误伤**。
//
// 最后一条是这个命令的安全边界：它要解决「API 令牌泄漏」，
// 不该把用户从浏览器里全部踢下线（那是 reset-password 的职责）。
func TestRotateBootstrapTokenRevokesOnlyAPIKind(t *testing.T) {
	ctx := context.Background()
	svc, main := newResetTestService(t)
	u := seedUser(t, ctx, main, "admin", "old-password-1")

	// 旧 api 令牌（模拟「曾被打印进 app.log」的那条）。
	oldAPI, _, err := svc.IssueToken(ctx, u.ID, model.TokenKindAPI, "admin")
	if err != nil {
		t.Fatalf("造旧 api 令牌失败: %v", err)
	}
	// 网页会话令牌：轮转 api 令牌时不该被动到。
	webTok, _, err := svc.IssueToken(ctx, u.ID, model.TokenKindWebSession, "write")
	if err != nil {
		t.Fatalf("造 web_session 令牌失败: %v", err)
	}

	plain, revoked, err := svc.RotateBootstrapToken(ctx)
	if err != nil {
		t.Fatalf("轮转失败: %v", err)
	}
	if revoked != 1 {
		t.Fatalf("应作废 1 个旧 api 令牌，实际 %d", revoked)
	}
	if plain == "" {
		t.Fatal("轮转必须返回新令牌明文")
	}

	// ① 旧 api 令牌失效。
	if _, err := svc.VerifyToken(ctx, oldAPI); err == nil {
		t.Fatal("旧 api 令牌应已失效")
	}
	// ② 新令牌可用。
	if _, err := svc.VerifyToken(ctx, plain); err != nil {
		t.Fatalf("新令牌必须可用: %v", err)
	}
	// ③ web_session 不被误伤（本命令的安全边界）。
	if _, err := svc.VerifyToken(ctx, webTok); err != nil {
		t.Fatalf("web_session 令牌不应被轮转影响: %v", err)
	}

	// ④ 库里有效 api 令牌恰好 1 条（新的那条）。
	n, err := main.Tokens().CountByKind(ctx, model.TokenKindAPI)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("轮转后有效 api 令牌应恰好 1 条，实际 %d", n)
	}
}

// 连续轮转两次都必须成功（幂等收敛：每次都只剩 1 条有效 api 令牌）。
func TestRotateBootstrapTokenTwiceIsStable(t *testing.T) {
	ctx := context.Background()
	svc, main := newResetTestService(t)
	seedUser(t, ctx, main, "admin", "old-password-1")

	first, _, err := svc.RotateBootstrapToken(ctx)
	if err != nil {
		t.Fatalf("首次轮转失败: %v", err)
	}
	second, revoked, err := svc.RotateBootstrapToken(ctx)
	if err != nil {
		t.Fatalf("二次轮转失败: %v", err)
	}
	if revoked != 1 {
		t.Fatalf("二次轮转应作废 1 条（首次签发的那条），实际 %d", revoked)
	}
	if first == second {
		t.Fatal("两次轮转的令牌明文不应相同")
	}
	if _, err := svc.VerifyToken(ctx, first); err == nil {
		t.Fatal("首次令牌在二次轮转后应失效")
	}
	if _, err := svc.VerifyToken(ctx, second); err != nil {
		t.Fatalf("最新令牌必须可用: %v", err)
	}
}

// RevokeByKind 的关键防线：kind 传空串必须被拒绝。
//
// 空 kind 会让 UPDATE 命中全部令牌（连同 web_session / sso 一起废掉），
// 属于「静默扩大副作用面」的高危误伤，必须在 store 层硬挡。
func TestRevokeByKindRejectsEmptyKind(t *testing.T) {
	ctx := context.Background()
	_, main := newResetTestService(t)
	u := seedUser(t, ctx, main, "admin", "old-password-1")
	svc := NewService(Options{Main: main, PasswordParams: TestParams()})
	tok, _, err := svc.IssueToken(ctx, u.ID, model.TokenKindAPI, "admin")
	if err != nil {
		t.Fatalf("造令牌失败: %v", err)
	}

	if _, err := main.Tokens().RevokeByKind(ctx, "", time.Now().UnixMilli()); err == nil {
		t.Fatal("空 kind 必须被拒绝")
	}
	// 拒绝之后令牌必须仍然有效（证明真的什么都没改）。
	if _, err := svc.VerifyToken(ctx, tok); err != nil {
		t.Fatalf("被拒绝的调用不应改动任何令牌: %v", err)
	}
}
