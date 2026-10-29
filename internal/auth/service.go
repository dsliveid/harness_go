package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// Service 账号与令牌用例。
type Service struct {
	main       *store.MainStore
	params     Params
	sessionTTL time.Duration
	now        func() time.Time
}

// Options 构造参数。
type Options struct {
	Main           *store.MainStore
	SessionTTL     time.Duration // web 会话有效期，默认 7d
	PasswordParams Params        // 默认 OWASP 推荐参数
	Now            func() time.Time
}

// NewService 创建账号用例。
func NewService(opts Options) *Service {
	if opts.SessionTTL <= 0 {
		opts.SessionTTL = 7 * 24 * time.Hour
	}
	if opts.PasswordParams.Memory == 0 {
		opts.PasswordParams = DefaultParams()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{main: opts.Main, params: opts.PasswordParams, sessionTTL: opts.SessionTTL, now: opts.Now}
}

// EnsureBootstrapUser 首启引导账号（仅在库内无账号时创建）。
//
// 口令为空则随机生成并返回（调用方负责打印/落文件，库内只存哈希）。
func (s *Service) EnsureBootstrapUser(ctx context.Context, username, password string) (generated string, created bool, err error) {
	n, err := s.main.Users().Count(ctx)
	if err != nil {
		return "", false, err
	}
	if n > 0 {
		return "", false, nil
	}
	if strings.TrimSpace(username) == "" {
		username = "admin"
	}
	if password == "" {
		generated = RandomPassword(20)
		password = generated
	}
	hash, err := HashPassword(password, s.params)
	if err != nil {
		return "", false, err
	}
	u := &model.User{
		ID:           newID(),
		Username:     username,
		PasswordHash: hash,
		DisplayName:  username,
		Role:         "owner",
	}
	if err := s.main.Users().Create(ctx, u); err != nil {
		return "", false, err
	}
	return generated, true, nil
}

// Login 校验口令并签发网页会话令牌。
func (s *Service) Login(ctx context.Context, username, password string) (*model.User, string, error) {
	u, err := s.main.Users().GetByUsername(ctx, strings.TrimSpace(username))
	if err != nil {
		// 用户不存在也执行一次哈希校验，抹平时序差异（防枚举）
		_, _ = VerifyPassword(password, dummyHash)
		return nil, "", apperr.New(apperr.CodeAuthInvalidCredentials, "用户名或口令错误")
	}
	ok, err := VerifyPassword(password, u.PasswordHash)
	if err != nil {
		return nil, "", apperr.Wrap(err, apperr.CodeAuthInvalidCredentials, "用户名或口令错误")
	}
	if !ok {
		return nil, "", apperr.New(apperr.CodeAuthInvalidCredentials, "用户名或口令错误")
	}
	token, _, err := s.IssueToken(ctx, u.ID, model.TokenKindWebSession, "write")
	if err != nil {
		return nil, "", err
	}
	return u, token, nil
}

// SSOLogin 免登：校验凭据并签发网页会话令牌（调用方负责 302 与 Set-Cookie）。
func (s *Service) SSOLogin(ctx context.Context, username, password string) (*model.User, string, error) {
	return s.Login(ctx, username, password)
}

// IssueSessionForUsername 按用户名签发网页会话（signed 模式专用）。
//
// 语义：外部系统已用密钥完成认证，此处不再校验口令，仅确认账号存在。
func (s *Service) IssueSessionForUsername(ctx context.Context, username string) (*model.User, string, error) {
	u, err := s.main.Users().GetByUsername(ctx, strings.TrimSpace(username))
	if err != nil {
		return nil, "", apperr.New(apperr.CodeAuthInvalidCredentials, "凭据无效")
	}
	token, _, err := s.IssueToken(ctx, u.ID, model.TokenKindSSO, "write")
	if err != nil {
		return nil, "", err
	}
	return u, token, nil
}

// Me 返回当前账号。
func (s *Service) Me(ctx context.Context, userID string) (*model.User, error) {
	u, err := s.main.Users().GetByID(ctx, userID)
	if err != nil {
		return nil, apperr.New(apperr.CodeUnauthorized, "账号不存在或已失效")
	}
	return u, nil
}

// ChangePassword 修改口令（需校验旧口令）。
func (s *Service) ChangePassword(ctx context.Context, userID, oldPassword, newPassword string) error {
	if len([]rune(newPassword)) < 8 {
		return apperr.New(apperr.CodeAuthPasswordPolicy, "新口令至少 8 个字符")
	}
	u, err := s.main.Users().GetByID(ctx, userID)
	if err != nil {
		return apperr.New(apperr.CodeUnauthorized, "账号不存在或已失效")
	}
	ok, err := VerifyPassword(oldPassword, u.PasswordHash)
	if err != nil || !ok {
		return apperr.New(apperr.CodeAuthInvalidCredentials, "原口令错误")
	}
	hash, err := HashPassword(newPassword, s.params)
	if err != nil {
		return err
	}
	return s.main.Users().UpdatePassword(ctx, userID, hash)
}

// IssueToken 签发令牌，返回明文（仅此一次）与记录。
func (s *Service) IssueToken(ctx context.Context, userID, kind, scope string) (string, *model.Token, error) {
	plain := "hgt_" + randomHex(24)
	expires := s.now().Add(s.sessionTTL).UnixMilli()
	tk := &model.Token{
		ID:        newID(),
		UserID:    userID,
		Name:      kind,
		Kind:      kind,
		TokenHash: HashToken(plain),
		Scope:     scope,
		ExpiresAt: &expires,
	}
	if err := s.main.Tokens().Create(ctx, tk); err != nil {
		return "", nil, err
	}
	return plain, tk, nil
}

// VerifyToken 校验明文令牌，返回账号 ID（供 api 中间件调用）。
func (s *Service) VerifyToken(ctx context.Context, plain string) (string, error) {
	plain = strings.TrimSpace(plain)
	if plain == "" {
		return "", apperr.New(apperr.CodeUnauthorized, "缺少访问令牌")
	}
	tk, err := s.main.Tokens().GetByHash(ctx, HashToken(plain))
	if err != nil {
		// 不区分「不存在」与「已吊销」，统一 401（防枚举）
		return "", apperr.New(apperr.CodeAuthInvalidCredentials, "访问令牌无效")
	}
	if tk.RevokedAt != nil {
		return "", apperr.New(apperr.CodeAuthInvalidCredentials, "访问令牌无效")
	}
	if tk.ExpiresAt != nil && *tk.ExpiresAt <= s.now().UnixMilli() {
		return "", apperr.New(apperr.CodeAuthSessionExpired, "会话已过期，请重新登录")
	}
	_ = s.main.Tokens().Touch(ctx, tk.ID, s.now().UnixMilli())
	if tk.UserID == "" {
		return "local", nil
	}
	return tk.UserID, nil
}

// Logout 吊销当前令牌。
func (s *Service) Logout(ctx context.Context, plain string) error {
	tk, err := s.main.Tokens().GetByHash(ctx, HashToken(strings.TrimSpace(plain)))
	if err != nil {
		return nil // 幂等：已失效视为登出成功
	}
	return s.main.Tokens().Revoke(ctx, tk.ID, s.now().UnixMilli())
}

// EnsureBootstrapToken 确保至少存在一个 API 令牌（供 curl / 外部接入使用）。
//
// 令牌会绑定到最早的账号（一期单用户），这样 Bearer 与 Cookie 两种通道
// 拿到的是同一个认证主体，`/auth/me`、改密等接口行为一致。
func (s *Service) EnsureBootstrapToken(ctx context.Context) (plain string, created bool, err error) {
	n, err := s.main.Tokens().CountByKind(ctx, model.TokenKindAPI)
	if err != nil {
		return "", false, err
	}
	if n > 0 {
		return "", false, nil
	}
	userID := ""
	if u, uErr := s.main.Users().First(ctx); uErr == nil {
		userID = u.ID
	}
	plain = "hgt_" + randomHex(24)
	tk := &model.Token{
		ID:        newID(),
		UserID:    userID,
		Name:      "bootstrap",
		Kind:      model.TokenKindAPI,
		TokenHash: HashToken(plain),
		Scope:     "admin",
	}
	if err := s.main.Tokens().Create(ctx, tk); err != nil {
		return "", false, err
	}
	return plain, true, nil
}

// HashToken 令牌哈希（sha256 hex）。库内永不存明文。
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// RandomPassword 生成随机口令（大小写 + 数字，避免歧义字符）。
func RandomPassword(n int) string {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	if n <= 0 {
		n = 16
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("pwd-%d", time.Now().UnixNano())
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("id-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// dummyHash 用于抹平「用户不存在」与「口令错误」的时序差异。
var dummyHash = mustDummyHash()

func mustDummyHash() string {
	h, err := HashPassword("dummy-password-for-timing", TestParams())
	if err != nil {
		return ""
	}
	return h
}

// ErrNotConfigured 未配置。
var ErrNotConfigured = errors.New("auth: 未配置")
