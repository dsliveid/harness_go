package auth

import (
	"strings"
	"testing"
	"time"
)

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("s3cret-pass", TestParams())
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	// 格式：$argon2id$v=19$m=...,t=...,p=...$salt$key
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Fatalf("哈希格式错误: %s", hash)
	}
	if strings.Contains(hash, "s3cret-pass") {
		t.Fatal("哈希串不得包含明文口令")
	}

	ok, err := VerifyPassword("s3cret-pass", hash)
	if err != nil || !ok {
		t.Fatalf("正确口令应校验通过: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword("wrong-pass", hash)
	if err != nil {
		t.Fatalf("校验不应报错: %v", err)
	}
	if ok {
		t.Fatal("错误口令不应通过")
	}

	// 同一口令两次哈希必须不同（salt 随机）
	hash2, _ := HashPassword("s3cret-pass", TestParams())
	if hash == hash2 {
		t.Fatal("salt 应随机，两次哈希不应相同")
	}
}

func TestHashPasswordRejectsEmpty(t *testing.T) {
	if _, err := HashPassword("", TestParams()); err == nil {
		t.Fatal("空口令应报错")
	}
}

func TestVerifyPasswordRejectsInvalidHash(t *testing.T) {
	cases := []string{
		"",
		"plaintext",
		"$argon2id$v=19$m=1024,t=1,p=1$onlyfourparts",
		"$bcrypt$v=19$m=1024,t=1,p=1$c2FsdA$a2V5",
		"$argon2id$v=99$m=1024,t=1,p=1$c2FsdA$a2V5",
		"$argon2id$v=19$m=x,t=1,p=1$c2FsdA$a2V5",
		"$argon2id$v=19$m=1024,t=1,p=1$!!!$a2V5",
	}
	for _, c := range cases {
		if _, err := VerifyPassword("x", c); err == nil {
			t.Fatalf("非法哈希应报错: %q", c)
		}
	}
}

func TestRandomPassword(t *testing.T) {
	p1 := RandomPassword(20)
	p2 := RandomPassword(20)
	if len(p1) != 20 {
		t.Fatalf("长度应为 20，得到 %d", len(p1))
	}
	if p1 == p2 {
		t.Fatal("两次生成不应相同")
	}
	// 不含易混淆字符
	for _, bad := range []string{"0", "O", "l", "1", "I"} {
		if strings.Contains(p1, bad) {
			t.Fatalf("不应包含易混淆字符 %q: %s", bad, p1)
		}
	}
	if RandomPassword(0) == "" {
		t.Fatal("n<=0 应回退默认长度")
	}
}

func TestSignAndVerify(t *testing.T) {
	secret := "sso-secret"
	now := time.Now()
	params := BuildSignedParams(secret, "admin", "10.0.0.1", now)

	if err := VerifySign(secret, "admin", params["ts"], "10.0.0.1", params["sign"], now, SignWindow); err != nil {
		t.Fatalf("签名应校验通过: %v", err)
	}

	// 用户名被改 → 不匹配
	if err := VerifySign(secret, "other", params["ts"], "10.0.0.1", params["sign"], now, SignWindow); err == nil {
		t.Fatal("用户名不匹配应失败")
	}
	// IP 被换 → 不匹配（防签名链接搬到另一台机器重放）
	if err := VerifySign(secret, "admin", params["ts"], "10.0.0.2", params["sign"], now, SignWindow); err == nil {
		t.Fatal("IP 不匹配应失败")
	}
	// 密钥错 → 不匹配
	if err := VerifySign("wrong-secret", "admin", params["ts"], "10.0.0.1", params["sign"], now, SignWindow); err == nil {
		t.Fatal("密钥错误应失败")
	}
}

func TestVerifySignWindow(t *testing.T) {
	secret := "s"
	now := time.Now()
	old := now.Add(-2 * SignWindow)
	params := BuildSignedParams(secret, "admin", "1.1.1.1", old)

	err := VerifySign(secret, "admin", params["ts"], "1.1.1.1", params["sign"], now, SignWindow)
	if err != ErrSignExpired {
		t.Fatalf("超窗应返回 ErrSignExpired，得到 %v", err)
	}
	// 时间戳非法
	if err := VerifySign(secret, "admin", "not-a-number", "1.1.1.1", params["sign"], now, SignWindow); err != ErrSignExpired {
		t.Fatalf("非法时间戳应返回 ErrSignExpired，得到 %v", err)
	}
	// 未配置密钥
	if err := VerifySign("", "admin", params["ts"], "1.1.1.1", params["sign"], now, SignWindow); err != ErrSignDisabled {
		t.Fatalf("未配置密钥应返回 ErrSignDisabled，得到 %v", err)
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(3, time.Minute)
	now := time.Now()
	l.SetNow(func() time.Time { return now })

	for i := 0; i < 3; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("第 %d 次应放行", i+1)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("超过上限应拒绝")
	}
	// 其他 IP 不受影响
	if !l.Allow("5.6.7.8") {
		t.Fatal("不同 key 应独立计数")
	}
	// 窗口滑过后恢复
	now = now.Add(2 * time.Minute)
	if !l.Allow("1.2.3.4") {
		t.Fatal("窗口滑过后应恢复")
	}
	l.Reset()
	if !l.Allow("1.2.3.4") {
		t.Fatal("Reset 后应放行")
	}
}

func TestHashToken(t *testing.T) {
	h1 := HashToken("hgt_abc")
	h2 := HashToken("hgt_abc")
	if h1 != h2 {
		t.Fatal("同一令牌哈希应稳定")
	}
	if len(h1) != 64 {
		t.Fatalf("sha256 hex 应为 64 字符，得到 %d", len(h1))
	}
	if strings.Contains(h1, "hgt_abc") {
		t.Fatal("哈希不得包含明文")
	}
}
