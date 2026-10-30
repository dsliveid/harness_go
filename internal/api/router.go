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
	h := &handlers{d: d, web: newAssets(d.Log)}
	mux := http.NewServeMux()

	// SPA 入口（同时承担 SSO 免登拦截）
	mux.HandleFunc("GET /{$}", h.root)
	// 前端静态资源（//go:embed web/dist）：带 hash 的产物 + PWA 资源
	mux.HandleFunc("GET /assets/", h.serveWebAsset)
	mux.HandleFunc("GET /icons/", h.serveWebAsset)
	mux.HandleFunc("GET /manifest.webmanifest", h.serveWebAsset)
	mux.HandleFunc("GET /sw.js", h.serveWebAsset)
	// SPA 路由（B-1 只有登录页与主页面两条，前端自行渲染）
	mux.HandleFunc("GET /login", h.serveLogin)

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
	// 审批决策（B-3）：aid = 等待中的审批 ID（= 工具事件 ID）
	mux.HandleFunc("POST /api/v1/sessions/{sid}/runs/{rid}/approvals/{aid}/decide", h.decideApproval)

	// 运维
	mux.HandleFunc("GET /api/v1/settings/maintenance", h.maintenance)

	// 厂商与配置（B-4）
	mux.HandleFunc("GET /api/v1/providers", h.listProviders)
	mux.HandleFunc("POST /api/v1/providers", h.createProvider)
	mux.HandleFunc("PATCH /api/v1/providers/{pid}", h.updateProvider)
	mux.HandleFunc("DELETE /api/v1/providers/{pid}", h.deleteProvider)
	mux.HandleFunc("POST /api/v1/providers/{pid}/test", h.testProvider)
	mux.HandleFunc("GET /api/v1/settings", h.getSettings)
	mux.HandleFunc("PATCH /api/v1/settings", h.patchSettings)
	mux.HandleFunc("GET /api/v1/stats", h.stats)
	mux.HandleFunc("GET /api/v1/logs", h.logs)

	// 工具（A-10 协议层可观测与注入配置）
	mux.HandleFunc("GET /api/v1/tools", h.listTools)
	mux.HandleFunc("GET /api/v1/tools/injection", h.toolsInjection)
	mux.HandleFunc("PATCH /api/v1/tools/tiers", h.patchToolTiers)

	return Chain(mux,
		Recover(d.Log),
		TraceID(),
		AccessLog(d.Log),
		Auth(d.VerifyToken, publicPaths...),
		RateLimit(20, 40),
	)
}
