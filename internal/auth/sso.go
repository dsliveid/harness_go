package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"
)

// SignWindow 签名时间窗口（防重放，docs/开发实施计划.md 附录 A）。
const SignWindow = 60 * time.Second

// ErrSignExpired 时间戳超窗或非法。
var ErrSignExpired = errors.New("auth: 签名已过期")

// ErrSignMismatch 签名不匹配。
var ErrSignMismatch = errors.New("auth: 签名不匹配")

// ErrSignDisabled 未配置签名密钥。
var ErrSignDisabled = errors.New("auth: 未配置 SSO 签名密钥")

// Sign 计算 SSO 签名：HMAC-SHA256(user_name + "\n" + ts + "\n" + ip, secret)。
//
// 用 ip 参与签名可防止签名链接被搬到另一台机器重放。
func Sign(secret, userName, ts, ip string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(userName))
	mac.Write([]byte("\n"))
	mac.Write([]byte(ts))
	mac.Write([]byte("\n"))
	mac.Write([]byte(ip))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySign 校验签名与时间窗口。
func VerifySign(secret, userName, ts, ip, sign string, now time.Time, window time.Duration) error {
	if secret == "" {
		return ErrSignDisabled
	}
	if window <= 0 {
		window = SignWindow
	}
	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrSignExpired
	}
	diff := now.Unix() - tsInt
	if diff < 0 {
		diff = -diff
	}
	if time.Duration(diff)*time.Second > window {
		return ErrSignExpired
	}
	expected := Sign(secret, userName, ts, ip)
	if !hmac.Equal([]byte(expected), []byte(sign)) {
		return ErrSignMismatch
	}
	return nil
}

// BuildSignedParams 生成带签名的查询参数（供外部系统跳转使用，测试与文档示例用）。
func BuildSignedParams(secret, userName, ip string, now time.Time) map[string]string {
	ts := strconv.FormatInt(now.Unix(), 10)
	return map[string]string{
		"user_name": userName,
		"ts":        ts,
		"sign":      Sign(secret, userName, ts, ip),
	}
}
