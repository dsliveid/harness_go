package observ

import (
	"strings"
	"testing"
)

// 报文脱敏是安全红线：用户经常把凭据直接粘进对话，而日志要记录报文原文。
// 现有 RedactQuery 只覆盖 URL query 白名单，报文 body 完全没有覆盖——这里必须补上。
func TestRedactPayloadFiveCredentialKinds(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		mustGone []string // 脱敏后必须消失的原文片段
	}{
		{
			name:     "sk 密钥",
			in:       `{"note":"key 是 sk-abcdefghijklmnop1234567890 请查"}`,
			mustGone: []string{"sk-abcdefghijklmnop1234567890"},
		},
		{
			name:     "Bearer token",
			in:       `Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9`,
			mustGone: []string{"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"},
		},
		{
			name:     "password 赋值",
			in:       `{"user":"admin","password":"hunter2"}`,
			mustGone: []string{"hunter2"},
		},
		{
			name:     "api_key 赋值",
			in:       `config api_key = "abc123secretvalue"`,
			mustGone: []string{"abc123secretvalue"},
		},
		{
			name: "PEM 私钥块",
			in: "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n" +
				"-----END RSA PRIVATE KEY-----",
			mustGone: []string{"MIIEowIBAAKCAQEA"},
		},
		{
			name:     "AWS access key",
			in:       `aws AKIAIOSFODNN7EXAMPLE done`,
			mustGone: []string{"AKIAIOSFODNN7EXAMPLE"},
		},
		{
			// 本项目自有令牌：用户把服务端签发的 hgt_/hgd_ 粘进对话时也必须脱敏，
			// 否则「这个 token 为什么 401」这类提问会把有效凭据写进日志库。
			name:     "本项目用户令牌 hgt_",
			in:       `帮我看看 hgt_0123456789abcdef0123456789abcdef0123456789abcdef 为什么 401`,
			mustGone: []string{"hgt_0123456789abcdef0123456789abcdef0123456789abcdef"},
		},
		{
			name:     "本项目设备令牌 hgd_",
			in:       `{"device_token":"hgd_0123456789abcdef0123456789abcdef"}`,
			mustGone: []string{"hgd_0123456789abcdef0123456789abcdef"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := RedactPayload(c.in)
			for _, gone := range c.mustGone {
				if strings.Contains(out, gone) {
					t.Fatalf("原文 %q 未被脱敏，输出: %s", gone, out)
				}
			}
			if !strings.Contains(out, "***") {
				t.Fatalf("脱敏后应含 *** 占位，输出: %s", out)
			}
		})
	}
}

// 幂等：对已脱敏文本再次调用不得进一步改变（否则重试/多路径脱敏会破坏内容）。
func TestRedactPayloadIdempotent(t *testing.T) {
	in := `{"password":"hunter2","api_key":"abc123secret","note":"普通文本"}`
	once := RedactPayload(in)
	twice := RedactPayload(once)
	if once != twice {
		t.Fatalf("脱敏不幂等:\n一次: %s\n二次: %s", once, twice)
	}
	// 普通文本不应被误伤
	if !strings.Contains(twice, "普通文本") {
		t.Fatalf("非凭据文本被误伤: %s", twice)
	}
}

// 空串与纯文本必须原样返回（避免对正常日志做无谓改写）。
func TestRedactPayloadNoSecret(t *testing.T) {
	in := "普通日志行，没有任何凭据"
	if out := RedactPayload(in); out != in {
		t.Fatalf("无凭据文本不应被改写，得到 %s", out)
	}
	if out := RedactPayload(""); out != "" {
		t.Fatalf("空串应返回空串，得到 %q", out)
	}
}

// ContainsSecret 用于告警（不用于脱敏），需能命中各类凭据。
func TestContainsSecret(t *testing.T) {
	if ContainsSecret("普通文本") {
		t.Fatal("普通文本不应判为含凭据")
	}
	if !ContainsSecret("Bearer abcdefgh12345678") {
		t.Fatal("Bearer token 应被判为含凭据")
	}
	if !ContainsSecret(`{"password":"x"}`) {
		t.Fatal("password 应被判为含凭据")
	}
}

// RedactPayloadBytes 与字符串版本必须一致（Blob 写入前调用）。
func TestRedactPayloadBytes(t *testing.T) {
	in := []byte(`{"password":"hunter2"}`)
	out := string(RedactPayloadBytes(in))
	if strings.Contains(out, "hunter2") {
		t.Fatalf("字节版未脱敏: %s", out)
	}
	if len(RedactPayloadBytes(nil)) != 0 {
		t.Fatal("nil 应返回 nil/空")
	}
}
