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

// ParseLevel 把配置字符串解析为 slog.Level。
//
// 空串与非法值一律回落 info（启动期容错），需要严格校验的写入路径请用
// config.ValidLogLevel 先行拒绝脏值，不要依赖本函数的兜底。
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "info":
		return slog.LevelInfo
	default:
		return slog.LevelInfo
	}
}

// NewLoggerWithLevel 创建可**运行时热调级别**的 logger，并返回级别句柄。
//
// 为什么用 slog.LevelVar 而不是 slog.Level：
//   - HandlerOptions.Level 一旦持有不可变 slog.Level，进程内再也无法改级别，
//     只能重启（这正是「控制台刷屏又不敢降级，因为降级会连日志面板一起失明」的根因）。
//   - slog.LevelVar 内部是 atomic，Level()/Set() 并发安全，改动对所有已派生的
//     logger 立即生效，无需重建 handler、无需重启。
//
// 注意：本项目控制台与 data/app.log 共用同一个 io.MultiWriter（见 cmd/harness-go/main.go），
// 因此一次 Set 同时作用于控制台与日志文件——这正是「界面与控制台显示同一套」的实现基础。
func NewLoggerWithLevel(level, format string, w io.Writer) (*slog.Logger, *slog.LevelVar) {
	if w == nil {
		w = os.Stdout
	}
	var lv slog.LevelVar
	lv.Set(ParseLevel(level))
	opts := &slog.HandlerOptions{Level: &lv}
	var h slog.Handler
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h), &lv
}

// NewLogger 按配置创建 slog logger。level 支持 debug/info/warn/error。
//
// 级别在此不可运行时变更；需要热调（服务端主流程）请用 NewLoggerWithLevel。
// 保留本函数是为了让测试与一次性脚本不必关心 LevelVar。
func NewLogger(level, format string, w io.Writer) *slog.Logger {
	l, _ := NewLoggerWithLevel(level, format, w)
	return l
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
