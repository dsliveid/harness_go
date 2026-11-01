package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/auth"
	"harness-go/internal/events"
	"harness-go/internal/service"
)

// Deps 传输层依赖（全部经 service/auth，不接触 store）。
type Deps struct {
	Version     string
	Log         *slog.Logger
	Sessions    *service.SessionService
	Runs        *service.RunService
	Maintenance *service.MaintenanceService
	Tools       *service.ToolService
	Providers   *service.ProviderService
	Settings    *service.SettingsService
	Logs        *service.LogService
	Devices     *service.DeviceService
	// MCP / Packages 外部插件子系统（阶段 E；nil 时相关端点返回 unavailable）
	MCP      *service.MCPService
	Packages *service.PackageService
	// Rollback 快照回滚（阶段 G / S10；nil 时相关端点返回 unavailable）
	Rollback *service.RollbackService
	// Webhooks / OpenAI 接入增强（P1；nil 时相关端点返回 unavailable）
	Webhooks *service.WebhookService
	OpenAI   *service.OpenAICompatService
	// DeviceWS 设备长连接网关（阶段 C-3；nil 时 /devices/ws 返回 503 device.disabled）
	DeviceWS    http.Handler
	Bus         *events.Bus
	Auth        *auth.Service
	AuthConfig  AuthConfig
	VerifyToken TokenVerifier
}

type handlers struct {
	d   Deps
	web *assets
}

// ============ 会话 ============

type createSessionReq struct {
	Title         string `json:"title"`
	ModelRef      string `json:"model_ref"`
	WorkspacePath string `json:"workspace_path"`
}

func (h *handlers) listSessions(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 50)
	offset := intParam(r, "offset", 0)
	list, err := h.d.Sessions.List(r.Context(), limit, offset)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list, "limit": limit, "offset": offset})
}

func (h *handlers) createSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	sess, err := h.d.Sessions.Create(r.Context(), req.Title, req.ModelRef, req.WorkspacePath)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (h *handlers) getSession(w http.ResponseWriter, r *http.Request) {
	sess, err := h.d.Sessions.Get(r.Context(), r.PathValue("sid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// updateSessionReq PATCH /api/v1/sessions/{sid} 入参（均可选）：
// title 非空时重命名；model_ref 用指针区分「未传」与「显式清空（未选择模型）」；
// access_mode 非空时更新访问模式（confirm/full，切换即时生效）。
type updateSessionReq struct {
	Title      string  `json:"title"`
	ModelRef   *string `json:"model_ref"`
	AccessMode string  `json:"access_mode"`
}

func (h *handlers) renameSession(w http.ResponseWriter, r *http.Request) {
	var req updateSessionReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if req.Title != "" {
		if err := h.d.Sessions.Rename(r.Context(), r.PathValue("sid"), req.Title); err != nil {
			writeError(w, r, err)
			return
		}
	}
	if req.ModelRef != nil {
		if err := h.d.Sessions.UpdateModel(r.Context(), r.PathValue("sid"), *req.ModelRef); err != nil {
			writeError(w, r, err)
			return
		}
	}
	if strings.TrimSpace(req.AccessMode) != "" {
		if err := h.d.Sessions.SetAccessMode(r.Context(), r.PathValue("sid"), req.AccessMode); err != nil {
			writeError(w, r, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) deleteSession(w http.ResponseWriter, r *http.Request) {
	if err := h.d.Sessions.Delete(r.Context(), r.PathValue("sid")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// archiveSession 归档会话：POST /api/v1/sessions/{sid}/archive（204；幂等）。
//
// 状态落主库 sessions.archived_at（0007 迁移），对齐桌面端「归档中心」：
// 侧栏默认列表不再显示，归档中心可查看/取消归档，删除语义不变。
func (h *handlers) archiveSession(w http.ResponseWriter, r *http.Request) {
	if err := h.d.Sessions.Archive(r.Context(), r.PathValue("sid")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// unarchiveSession 取消归档：POST /api/v1/sessions/{sid}/unarchive（204；幂等）。
func (h *handlers) unarchiveSession(w http.ResponseWriter, r *http.Request) {
	if err := h.d.Sessions.Unarchive(r.Context(), r.PathValue("sid")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) listMessages(w http.ResponseWriter, r *http.Request) {
	afterSeq := int64(intParam(r, "after_seq", 0))
	limit := intParam(r, "limit", 100)
	msgs, err := h.d.Sessions.Messages(r.Context(), r.PathValue("sid"), afterSeq, limit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": msgs, "after_seq": afterSeq, "limit": limit})
}

// ============ Run ============

type createRunReq struct {
	Input    string `json:"input"`
	ModelRef string `json:"model_ref"`
	// DeviceID 执行端设备（阶段 C-6，可选）：非空表示该 Run 以该设备为会话租约持有者，
	// 抢占旧端 epoch+1 并推送 lease.revoked；缺省/空串 = 服务端本地执行面。
	DeviceID string `json:"device_id,omitempty"`
}

func (h *handlers) createRun(w http.ResponseWriter, r *http.Request) {
	var req createRunReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	run, err := h.d.Runs.CreateRunAs(r.Context(), r.PathValue("sid"), req.Input, req.ModelRef, req.DeviceID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// 202：Run 已受理但仍在执行，客户端应订阅 events 获取流式输出
	writeJSON(w, http.StatusAccepted, map[string]any{
		"run_id": run.ID,
		"status": string(run.Status),
	})
}

func (h *handlers) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := h.d.Runs.GetRun(r.Context(), r.PathValue("sid"), r.PathValue("rid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (h *handlers) stopRun(w http.ResponseWriter, r *http.Request) {
	if err := h.d.Runs.StopRun(r.Context(), r.PathValue("rid")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// guidanceReq 运行中引导入参。
type guidanceReq struct {
	Input string `json:"input"`
}

// guideRun 运行中引导：POST /api/v1/sessions/{sid}/runs/{rid}/guidance
//
// 用户消息立即落库（响应返回该消息，前端就地追加），引擎在轮次边界回灌上下文——
// 当前流结束后模型即看到该引导。Run 不在运行中 → 409。
func (h *handlers) guideRun(w http.ResponseWriter, r *http.Request) {
	var req guidanceReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	msg, err := h.d.Runs.Guidance(r.Context(), r.PathValue("sid"), r.PathValue("rid"), req.Input)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, msg)
}

func (h *handlers) listToolEvents(w http.ResponseWriter, r *http.Request) {
	evs, err := h.d.Runs.ToolEvents(r.Context(), r.PathValue("sid"), r.PathValue("rid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": evs})
}

// runEvents SSE 端点：支持 Last-Event-ID 断线续传。
func (h *handlers) runEvents(w http.ResponseWriter, r *http.Request) {
	sid, rid := r.PathValue("sid"), r.PathValue("rid")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, apperr.New(apperr.CodeInternal, "当前连接不支持流式响应"))
		return
	}

	var fromSeq int64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			fromSeq = n
		}
	} else if v := r.URL.Query().Get("last_event_id"); v != "" {
		// EventSource 无法自定义请求头，浏览器侧续传走 query（见 web/src/lib/sse.ts）
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			fromSeq = n
		}
	}

	sub, err := h.d.Bus.Subscribe(rid, fromSeq)
	if err != nil {
		writeError(w, r, apperr.Wrap(err, apperr.CodeInvalidArgument, "订阅事件失败"))
		return
	}
	defer h.d.Bus.Unsubscribe(sub)

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 反代禁用缓冲
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case ev, open := <-sub.C():
			if !open {
				// Run 终态或慢消费者：发 resync 让客户端对齐后退出
				h.writeResync(w, r, sid, rid, sub.CloseReason())
				flusher.Flush()
				return
			}
			if err := writeSSEFrame(w, ev); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if err := writeSSEComment(w, "ping"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// writeResync 发送对齐事件（含 Run 终态与最后 seq），客户端据此决定是否重拉消息快照。
func (h *handlers) writeResync(w io.Writer, r *http.Request, sid, rid string, reason events.CloseReason) {
	lastSeq := h.d.Bus.LastSeq(rid)
	payload := map[string]any{
		"last_seq":     lastSeq,
		"close_reason": string(reason),
	}
	if run, err := h.d.Runs.GetRun(r.Context(), sid, rid); err == nil {
		payload["run_status"] = string(run.Status)
	}
	ev, err := events.NewEvent(rid, lastSeq, events.TypeResync, payload)
	if err != nil {
		return
	}
	_ = writeSSEFrame(w, ev)
}

// ============ 运维 ============

func (h *handlers) maintenance(w http.ResponseWriter, r *http.Request) {
	stats, err := h.d.Maintenance.Stats(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// ============ 辅助 ============

// decodeJSON 解析请求体（拒绝未知字段，避免前后端字段漂移被静默忽略）。
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apperr.Wrap(err, apperr.CodeInvalidArgument, "请求体格式错误")
	}
	return nil
}

func intParam(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
