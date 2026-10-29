// Package auth 提供密码哈希、令牌签发校验、单点登录签名与账号用例。
//
// 安全铁律（docs/开发实施计划.md 附录 A）：
//   - 口令用 argon2id 哈希，库内永不明文
//   - 令牌库内只存 sha256，明文仅在签发时出现一次
//   - SSO 凭据剥离由**服务端**完成（302 到干净 URL），不在前端 JS 解析
//   - 失败统一 401，不区分「用户不存在 / 口令错误」（防账号枚举）
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// ErrInvalidHash 哈希串格式非法。
var ErrInvalidHash = errors.New("auth: 口令哈希格式非法")

// ErrIncompatibleVersion argon2 版本不匹配。
var ErrIncompatibleVersion = errors.New("auth: argon2 版本不兼容")

// Params argon2id 参数。
type Params struct {
	Memory      uint32 // KiB
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// DefaultParams OWASP 推荐参数（m=64MiB, t=1, p=4）。
//
// 测试中可传入低参数版本以加快执行（见 TestParams）。
func DefaultParams() Params {
	return Params{
		Memory:      64 * 1024,
		Iterations:  1,
		Parallelism: 4,
		SaltLength:  16,
		KeyLength:   32,
	}
}

// TestParams 低开销参数（仅测试使用）。
func TestParams() Params {
	return Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}
}

// HashPassword 生成 argon2id 编码串（含参数与 salt，可直接存库）。
//
// 格式：$argon2id$v=19$m=65536,t=1,p=4$<b64 salt>$<b64 hash>
func HashPassword(password string, p Params) (string, error) {
	if password == "" {
		return "", errors.New("auth: 口令不能为空")
	}
	if p.Memory == 0 || p.Iterations == 0 || p.Parallelism == 0 {
		p = DefaultParams()
	}
	salt := make([]byte, p.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: 生成 salt 失败: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword 校验口令（参数从编码串中解析，兼容历史参数变更）。
//
// 使用常量时间比较，避免时序侧信道。
func VerifyPassword(password, encoded string) (bool, error) {
	p, salt, key, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	candidate := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLength)
	if subtle.ConstantTimeCompare(candidate, key) == 1 {
		return true, nil
	}
	return false, nil
}

func decodeHash(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, key]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return Params{}, nil, nil, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return Params{}, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return Params{}, nil, nil, ErrIncompatibleVersion
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Iterations, &p.Parallelism); err != nil {
		return Params{}, nil, nil, ErrInvalidHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, ErrInvalidHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, ErrInvalidHash
	}
	p.SaltLength = uint32(len(salt))
	p.KeyLength = uint32(len(key))
	return p, salt, key, nil
}
