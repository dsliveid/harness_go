// Package observ 提供日志、trace_id 与健康检查等横切能力。
//
// 约定（docs/架构设计文档.md §10.2）：
//   - 日志统一走 slog，字段结构化；敏感字段（pwd/sign/Authorization/api_key）永不落日志
//   - trace_id 由 TraceID 中间件注入 context，贯穿 service/agent/工具调用
//   - /healthz 暴露 version 与 protocol_version（多端版本协商依赖后者）
package observ

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// ProtocolVersion 是事件协议 / API 契约 / 设备协议的公共版本号。
// 多端版本漂移风险（docs/多端协同架构可行性方案.md 风险 5）靠它做双代兼容。
const ProtocolVersion = "1"

// startedAt 用于计算 uptime。
var startedAt = time.Now()

type ctxKey string

const traceKey ctxKey = "trace_id"

// NewTraceID 生成 16 位十六进制 trace_id。
func NewTraceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// WithTrace 把 trace_id 写入 context。
func WithTrace(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceKey, id)
}

// TraceFrom 从 context 取 trace_id（缺失返回空串）。
func TraceFrom(ctx context.Context) string {
	if v, ok := ctx.Value(traceKey).(string); ok {
		return v
	}
	return ""
}

// NewLogger 按配置创建 slog logger。level 支持 debug/info/warn/error。
func NewLogger(level, format string, w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stdout
	}
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
}

// healthBody 是 /healthz 响应体（字段名属外部契约）。
type healthBody struct {
	Status          string `json:"status"`
	Version         string `json:"version"`
	ProtocolVersion string `json:"protocol_version"`
	UptimeS         int64  `json:"uptime_s"`
}

// Healthz 返回健康检查 handler。
// 注意：不暴露数据目录、DB 路径等敏感信息。
func Healthz(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body := healthBody{
			Status:          "ok",
			Version:         version,
			ProtocolVersion: ProtocolVersion,
			UptimeS:         int64(time.Since(startedAt).Seconds()),
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(body)
	}
}
