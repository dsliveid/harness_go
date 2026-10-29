package api

import (
	"net/http"

	"harness-go/internal/observ"
)

// 免鉴权路径（登录与 SSO 必须在鉴权之前可达，否则无法登录）。
var publicPaths = []string{
	"/api/v1/auth/login",
	"/api/v1/auth/sso",
}

// NewRouter 构建路由（标准库 ServeMux，Go 1.22+ 路径参数）。
//
// 中间件顺序（外 → 内）：Recover → TraceID → AccessLog → Auth → RateLimit → Handler
func NewRouter(d Deps) http.Handler {
	h := &handlers{d: d}
	mux := http.NewServeMux()

	// SPA 入口（同时承担 SSO 免登拦截）
	mux.HandleFunc("GET /{$}", h.root)

	// 健康检查（免鉴权，供容器/负载均衡探活）
	mux.Handle("GET /healthz", observ.Healthz(d.Version))

	// 认证
	mux.HandleFunc("POST /api/v1/auth/login", h.login)
	mux.HandleFunc("POST /api/v1/auth/sso", h.ssoLogin)
	mux.HandleFunc("GET /api/v1/auth/me", h.me)
	mux.HandleFunc("POST /api/v1/auth/logout", h.logout)
	mux.HandleFunc("POST /api/v1/auth/password", h.changePassword)

	// 会话
	mux.HandleFunc("GET /api/v1/sessions", h.listSessions)
	mux.HandleFunc("POST /api/v1/sessions", h.createSession)
	mux.HandleFunc("GET /api/v1/sessions/{sid}", h.getSession)
	mux.HandleFunc("PATCH /api/v1/sessions/{sid}", h.renameSession)
	mux.HandleFunc("DELETE /api/v1/sessions/{sid}", h.deleteSession)
	mux.HandleFunc("GET /api/v1/sessions/{sid}/messages", h.listMessages)

	// Run（嵌套在会话下：分库架构中 Run 必然属于某个会话，避免跨库查找）
	mux.HandleFunc("POST /api/v1/sessions/{sid}/runs", h.createRun)
	mux.HandleFunc("GET /api/v1/sessions/{sid}/runs/{rid}", h.getRun)
	mux.HandleFunc("POST /api/v1/sessions/{sid}/runs/{rid}/stop", h.stopRun)
	mux.HandleFunc("GET /api/v1/sessions/{sid}/runs/{rid}/events", h.runEvents)
	mux.HandleFunc("GET /api/v1/sessions/{sid}/runs/{rid}/tool-events", h.listToolEvents)

	// 运维
	mux.HandleFunc("GET /api/v1/settings/maintenance", h.maintenance)

	return Chain(mux,
		Recover(d.Log),
		TraceID(),
		AccessLog(d.Log),
		Auth(d.VerifyToken, publicPaths...),
		RateLimit(20, 40),
	)
}
