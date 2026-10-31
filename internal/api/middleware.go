// Package api 是传输层：路由、中间件、handler、SSE 编码。
//
// 分层约束：api **不得 import store**（depguard 强制），所有数据访问经 service。
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/observ"
)

// Middleware 中间件签名。
type Middleware func(http.Handler) http.Handler

// Chain 按「外 → 内」顺序组合中间件。
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// statusRecorder 记录状态码与字节数（AccessLog 用），并透传 Flush（SSE 必需）。
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Flush 透传底层 Flusher（否则 SSE 无法实时推送）。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 暴露底层 ResponseWriter：coder/websocket 的升级依赖 http.Hijacker，
// 它通过 Unwrap 逐层向下查找（同 http.ResponseController 语义）。
// **缺此方法会导致 WS 升级失败并返回 501**。
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Recover 捕获 panic → 500，不向客户端泄露堆栈。
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic recovered",
						"panic", rec,
						"path", r.URL.Path,
						"trace_id", observ.TraceFrom(r.Context()))
					writeError(w, r, apperr.New(apperr.CodeInternal, "服务内部错误"))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// TraceID 生成或透传 trace_id，并回写响应头便于排障。
func TraceID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Trace-Id")
			if id == "" {
				id = observ.NewTraceID()
			}
			w.Header().Set("X-Trace-Id", id)
			next.ServeHTTP(w, r.WithContext(observ.WithTrace(r.Context(), id)))
		})
	}
}

// AccessLog 访问日志。**query 必须脱敏**：pwd / sign 永不落日志。
func AccessLog(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			log.Info("access",
				"method", r.Method,
				"path", r.URL.Path,
				"query", observ.RedactQuery(r.URL.RawQuery),
				"status", rec.status,
				"bytes", rec.bytes,
				"ms", time.Since(start).Milliseconds(),
				"trace_id", observ.TraceFrom(r.Context()),
				"ip", clientIP(r),
			)
		})
	}
}

// TokenVerifier 令牌校验（由 service 实现，api 不感知存储）。
type TokenVerifier func(ctx context.Context, token string) (userID string, err error)

type ctxKey string

const userIDKey ctxKey = "user_id"

// WithUserID 把认证主体写入 context。
func WithUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, userIDKey, id)
}

// UserID 读取认证主体。
func UserID(ctx context.Context) string {
	if v, ok := ctx.Value(userIDKey).(string); ok {
		return v
	}
	return ""
}

// Auth 鉴权：/api/** 必须携带有效令牌；publicPaths 中的路径放行（如登录、SSO）。
func Auth(verify TokenVerifier, publicPaths ...string) Middleware {
	public := make(map[string]bool, len(publicPaths))
	for _, p := range publicPaths {
		public[p] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/api/") || public[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}
			token := extractToken(r)
			if token == "" {
				writeError(w, r, apperr.New(apperr.CodeUnauthorized, "缺少访问令牌"))
				return
			}
			userID, err := verify(r.Context(), token)
			if err != nil {
				writeError(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithUserID(r.Context(), userID)))
		})
	}
}

// RateLimit 按 IP 的令牌桶限流（超限 429 + Retry-After）。
func RateLimit(rps, burst int) Middleware {
	if rps <= 0 {
		rps = 20
	}
	if burst <= 0 {
		burst = 40
	}
	type bucket struct {
		tokens float64
		last   time.Time
	}
	var mu sync.Mutex
	buckets := make(map[string]*bucket)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			now := time.Now()

			mu.Lock()
			b, ok := buckets[ip]
			if !ok {
				b = &bucket{tokens: float64(burst), last: now}
				buckets[ip] = b
			}
			elapsed := now.Sub(b.last).Seconds()
			b.tokens += elapsed * float64(rps)
			if b.tokens > float64(burst) {
				b.tokens = float64(burst)
			}
			b.last = now
			allowed := b.tokens >= 1
			if allowed {
				b.tokens--
			}
			mu.Unlock()

			if !allowed {
				w.Header().Set("Retry-After", "1")
				writeError(w, r, apperr.New(apperr.CodeRateLimited, "请求过于频繁，请稍后再试"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeJSON 写出 JSON 响应（统一 no-store）。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// writeError 写出统一错误体（自动注入 trace_id）。
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	ae := apperr.From(err)
	if ae == nil {
		ae = apperr.New(apperr.CodeInternal, "服务内部错误")
	}
	if trace := observ.TraceFrom(r.Context()); trace != "" && ae.TraceID == "" {
		ae = ae.WithTrace(trace)
	}
	writeJSON(w, ae.Status(), ae.Body())
}

// bearerToken 提取 Bearer 令牌（大小写不敏感）。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// extractToken 提取访问令牌：优先 Authorization: Bearer，其次会话 Cookie。
//
// 说明：Cookie 为 HttpOnly，前端 JS 读不到，只能由浏览器自动携带；
// 这是 SSO 免登与网页登录的默认通道。外部脚本/curl 走 Bearer。
func extractToken(r *http.Request) string {
	if t := bearerToken(r); t != "" {
		return t
	}
	if c, err := r.Cookie(SessionCookieName); err == nil {
		return strings.TrimSpace(c.Value)
	}
	return ""
}

// clientIP 提取客户端 IP（不信任 X-Forwarded-For，避免伪造绕过限流）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
