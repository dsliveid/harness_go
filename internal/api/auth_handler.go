package api

import (
	"net/http"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/auth"
	"harness-go/internal/model"
)

// SessionCookieName 网页会话 Cookie 名。
const SessionCookieName = "hg_sess"

// AuthConfig 认证相关配置（由 main 从 config 注入）。
type AuthConfig struct {
	SSOEnabled   bool
	SSOMode      string // plain | signed | both
	SSOSecret    string
	SessionTTL   time.Duration
	SecureCookie bool          // https 部署时置 true
	SSOLimiter   *auth.Limiter // SSO 入口独立限流（默认 5 次/分钟/IP）
}

// setSessionCookie 写入会话 Cookie（HttpOnly + SameSite=Lax，https 自动 Secure）。
func setSessionCookie(w http.ResponseWriter, token string, ttl time.Duration, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   int(ttl.Seconds()),
	})
}

// clearSessionCookie 清除会话 Cookie。
func clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   -1,
	})
}

// authUser 对外暴露的账号信息（**绝不包含口令哈希**）。
type authUser struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

func toAuthUser(u *model.User) authUser {
	return authUser{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: u.Role}
}

// ============ 端点 ============

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// login 表单登录（返回令牌并写 Cookie，前端可任选其一）。
func (h *handlers) login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	user, token, err := h.d.Auth.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		writeError(w, r, err)
		return
	}
	setSessionCookie(w, token, h.d.AuthConfig.SessionTTL, h.d.AuthConfig.SecureCookie)
	writeJSON(w, http.StatusOK, map[string]any{"user": toAuthUser(user), "token": token})
}

// me 首屏静默探测（前端用它避免闪登录页）。
func (h *handlers) me(w http.ResponseWriter, r *http.Request) {
	userID := UserID(r.Context())
	user, err := h.d.Auth.Me(r.Context(), userID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": toAuthUser(user)})
}

// logout 吊销当前令牌并清 Cookie（幂等）。
func (h *handlers) logout(w http.ResponseWriter, r *http.Request) {
	token := extractToken(r)
	if token != "" {
		_ = h.d.Auth.Logout(r.Context(), token)
	}
	clearSessionCookie(w, h.d.AuthConfig.SecureCookie)
	w.WriteHeader(http.StatusNoContent)
}

type changePasswordReq struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// changePassword 修改口令。
func (h *handlers) changePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if err := h.d.Auth.ChangePassword(r.Context(), UserID(r.Context()), req.OldPassword, req.NewPassword); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type ssoReq struct {
	UserName string `json:"user_name"`
	Pwd      string `json:"pwd"`
}

// ssoLogin 前端兜底自愈路径：vite dev / 纯静态部署时服务端不拦截 URL。
//
// 与「服务端 302 剥离」共用同一套校验逻辑，只是由谁负责剥离 URL 不同。
func (h *handlers) ssoLogin(w http.ResponseWriter, r *http.Request) {
	var req ssoReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if !h.d.AuthConfig.SSOEnabled {
		writeError(w, r, apperr.New(apperr.CodeAuthSSODisabled, "免登入口已关闭"))
		return
	}
	if h.d.AuthConfig.SSOLimiter != nil && !h.d.AuthConfig.SSOLimiter.Allow(clientIP(r)) {
		writeError(w, r, apperr.New(apperr.CodeAuthRateLimited, "尝试过于频繁，请稍后再试"))
		return
	}
	user, token, err := h.d.Auth.SSOLogin(r.Context(), req.UserName, req.Pwd)
	if err != nil {
		writeError(w, r, err)
		return
	}
	setSessionCookie(w, token, h.d.AuthConfig.SessionTTL, h.d.AuthConfig.SecureCookie)
	writeJSON(w, http.StatusOK, map[string]any{"user": toAuthUser(user)})
}
