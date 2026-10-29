package api

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/auth"
)

// rootHandler 是 SPA 入口，同时承担**单点登录免登拦截**。
//
// 铁律（docs/开发实施计划.md 附录 A）：URL 里的明文口令会污染浏览器历史、
// Referer、access log、代理日志，因此**必须由服务端校验并 302 到干净 URL**，
// 绝不交给前端 JS 解析。
//
//	GET /?user_name=xxx&pwd=yyy            → plain 模式
//	GET /?user_name=xxx&ts=..&sign=..      → signed 模式
//	→ Set-Cookie: hg_sess=<token>; HttpOnly → 302 /（不含凭据）
func (h *handlers) root(w http.ResponseWriter, r *http.Request) {
	// 所有 HTML 响应统一防 Referer 泄露 + 不缓存
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")

	userName := r.URL.Query().Get("user_name")
	if userName == "" {
		// 无凭据：正常入口。A 阶段前端尚未迁入（阶段 B），先返回提示页。
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<title>harness-go</title></head><body style="font-family:system-ui;padding:2rem;line-height:1.7">
<h1>harness-go</h1>
<p>服务端已就绪。前端将在阶段 B 从 <code>demo/</code> 迁入 <code>web/</code> 后由本入口提供。</p>
<p>可用 <code>GET /healthz</code> 检查健康状态，或用 <code>Authorization: Bearer &lt;token&gt;</code> 调用 <code>/api/v1/*</code>。</p>
</body></html>`)
		return
	}

	h.handleSSO(w, r, userName)
}

// handleSSO 处理免登入口。
func (h *handlers) handleSSO(w http.ResponseWriter, r *http.Request, userName string) {
	cfg := h.d.AuthConfig

	// ① 开关（server/cloud 形态默认关闭）
	if !cfg.SSOEnabled {
		h.ssoFail(w, r, apperr.New(apperr.CodeAuthSSODisabled, "免登入口已关闭"))
		return
	}

	ip := clientIP(r)

	// ② 独立限流（5 次/分钟/IP，比普通 API 更严）
	if cfg.SSOLimiter != nil && !cfg.SSOLimiter.Allow(ip) {
		h.ssoFail(w, r, apperr.New(apperr.CodeAuthRateLimited, "尝试过于频繁，请稍后再试"))
		return
	}

	// ③ 凭据校验（按模式）
	var (
		token string
		err   error
	)
	q := r.URL.Query()
	switch cfg.SSOMode {
	case "signed":
		if signErr := auth.VerifySign(cfg.SSOSecret, userName, q.Get("ts"), ip, q.Get("sign"), time.Now(), auth.SignWindow); signErr != nil {
			h.ssoFail(w, r, signError(signErr))
			return
		}
		_, token, err = h.d.Auth.IssueSessionForUsername(r.Context(), userName)
	case "both":
		// 有 sign 走签名，否则退回口令
		if q.Get("sign") != "" {
			if signErr := auth.VerifySign(cfg.SSOSecret, userName, q.Get("ts"), ip, q.Get("sign"), time.Now(), auth.SignWindow); signErr != nil {
				h.ssoFail(w, r, signError(signErr))
				return
			}
			_, token, err = h.d.Auth.IssueSessionForUsername(r.Context(), userName)
		} else {
			_, token, err = h.d.Auth.SSOLogin(r.Context(), userName, q.Get("pwd"))
		}
	default: // plain
		_, token, err = h.d.Auth.SSOLogin(r.Context(), userName, q.Get("pwd"))
	}
	if err != nil {
		// 失败统一 401，不区分「用户不存在 / 口令错误」
		h.ssoFail(w, r, apperr.New(apperr.CodeAuthInvalidCredentials, "凭据无效"))
		return
	}

	// ④ 写会话 Cookie
	setSessionCookie(w, token, cfg.SessionTTL, cfg.SecureCookie)

	// ⑤ 302 到不含凭据的干净 URL（地址栏与历史一次性干净）
	target := safeRedirect(q.Get("redirect"))
	http.Redirect(w, r, target, http.StatusFound)
}

// ssoFail 免登失败：**清空地址栏凭据**（重定向到 /?sso_error=1）并保留错误语义。
//
// 说明：失败时不回 401 而是 302 到登录页，是为了让浏览器地址栏立刻变干净
// （口令不出现在历史里）；前端据 sso_error 提示「凭据无效」。
func (h *handlers) ssoFail(w http.ResponseWriter, r *http.Request, err error) {
	_ = err // 错误细节不写入 URL（避免任何形式的凭据/原因泄露）
	http.Redirect(w, r, "/?sso_error=1", http.StatusFound)
}

// safeRedirect 只允许站内相对路径，防开放重定向。
func safeRedirect(raw string) string {
	if raw == "" {
		return "/"
	}
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.Contains(raw, "\\") {
		return "/"
	}
	return raw
}

// signError 把签名错误映射为对外错误码。
func signError(err error) error {
	switch {
	case errors.Is(err, auth.ErrSignExpired):
		return apperr.Wrap(err, apperr.CodeAuthSignExpired, "签名已过期，请重新发起")
	case errors.Is(err, auth.ErrSignDisabled):
		return apperr.Wrap(err, apperr.CodeAuthSSODisabled, "未配置签名密钥")
	default:
		return apperr.Wrap(err, apperr.CodeAuthInvalidCredentials, "凭据无效")
	}
}
