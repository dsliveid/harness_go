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
	"harness-go/internal/device"
	"harness-go/internal/events"
	"harness-go/internal/logvault"
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
	// LogConfig 报文抓取档位与保留天数的运行时配置（nil 时相关端点降级）
	LogConfig *service.LogConfigService
	// Vault 审计日志天分片库族（nil 时审计端点返回 unavailable）
	Vault *logvault.Querier
	// LogWriter 审计日志写入管道（仅用于暴露 dropped/written 诊断计数）
	LogWriter *logvault.Writer
	// Shard 天分片管理器（暴露 AvailableRange/TotalSizeBytes 给界面明示保留边界）
	Shard   *logvault.Shard
	Devices *service.DeviceService
	// MCP / Packages 外部插件子系统（阶段 E；nil 时相关端点返回 unavailable）
	MCP      *service.MCPService
	Packages *service.PackageService
	// Rollback 快照回滚（阶段 G / S10；nil 时相关端点返回 unavailable）
	Rollback *service.RollbackService
	// Artifacts 制品下载（R4：按快照 CAS 引用寻址；nil 时相关端点返回 unavailable）
	Artifacts *service.ArtifactService
	// Bundles 会话导出包（R2 会话导出；nil 时相关端点返回 unavailable）
	Bundles *service.BundleService
	// Backups 全量备份 / 恢复（R2；nil 时相关端点返回 unavailable）
	Backups *service.BackupService
	// Webhooks / OpenAI 接入增强（P1；nil 时相关端点返回 unavailable）
	Webhooks *service.WebhookService
	OpenAI   *service.OpenAICompatService
	// Projects 项目域（项目 CRUD + 默认控制权限 + 项目审批规则；nil 时返回 unavailable）
	Projects *service.ProjectService
	// Workspaces 工作空间域（执行面实体 CRUD + 在线/宿主派生字段；nil 时返回 unavailable）
	Workspaces *service.WorkspaceService
	// SOP 核心 SOP（agent-core 出厂内置 + 运行时覆盖；nil 时返回 unavailable）
	SOP *service.SOPService
	// FSRoots 允许浏览的目录根白名单（工作区宿主目录列举，L0 服务端宿主）：
	// 由 cmd 注入（HARNESS_FS_ROOTS + 数据目录兜底，均已 Abs）。为空表示**拒绝列举**
	// （失败即关闭，绝不放行全盘）。docs/工作区宿主与目录选择对接约定.md §4
	FSRoots []string
	// DeviceWS 设备长连接网关（阶段 C-3；nil 时 /devices/ws 返回 503 device.disabled）
	DeviceWS http.Handler
	// DeviceState 设备状态广播总线（nil 时 /devices/events 返回 503 device.disabled）。
	// 由 cmd 装配：把 Hub 的 OnOnline/OnOffline 与 StateBus.MarkOnline/MarkOffline 组合。
	DeviceState *device.StateBus
	Bus         *events.Bus
	Auth        *auth.Service
	AuthConfig  AuthConfig
	VerifyToken TokenVerifier
}

type handlers struct {
	d   Deps
	web *assets
	// webviewTickets WebView 免登一次性 ticket 内存表（协议 §6.2）。
	// 不落库：重启即失效可接受；无独立 GC 协程，签发时顺手清理过期项。
	webviewTickets *webviewTicketStore
}

// ============ 会话 ============

type createSessionReq struct {
	Title    string `json:"title"`
	ModelRef string `json:"model_ref"`
	// WorkspacePath 是**项目目录**（磁盘落盘路径），不是工作空间！
	// 与 WorkspaceID（执行面归属）完全无关，仅命名同带 Workspace 前缀。
	// 术语定义见 docs/术语表-工作空间与项目目录.md。
	WorkspacePath string `json:"workspace_path"`
	// ProjectID 归属项目；非空且未显式传 access_mode 时，服务端按项目默认控制权限快照继承。
	ProjectID string `json:"project_id"`
	// WorkspaceID 会话所属**工作空间**（执行面，协议 §3.3）：空 = 纯对话（不允许工具）。
	// 留空且 ProjectID 非空时，从项目的 workspace_id 继承。
	// 规划：将改为必填（纯对话亦须绑定，缺省绑服务端内置空间）。
	WorkspaceID string `json:"workspace_id"`
	// AccessMode 可选显式覆盖（confirm|full）；留空 = 走项目默认权限继承。
	AccessMode string `json:"access_mode"`
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
	// 能力天花板前置约束（协议 §4.1）：移动端 WebView 会话不得创建带执行面的会话。
	//
	// 用令牌 kind 判定，不用请求体自报（自报可伪造）。webview 会话能做的只有对话：
	// 强制清空 project_id / workspace_path，并锁 access_mode=confirm，双保险防提权。
	//
	// 🚨 注意：这里清空的 workspace_path 是**项目目录**（磁盘落盘路径），
	// **不是** WorkspaceID（工作空间/执行面归属）——req.WorkspaceID 不在此处被清空。
	// 二者仅命名同带 Workspace 前缀，语义完全无关，详见 docs/术语表-工作空间与项目目录.md。
	//
	// 注：这是「端侧自律 + 服务端兜底」的组合。真正不可绕过的是 run_service.resolveToolPolicy
	// ——即便有人绕过此处建了 mobile 工作空间的会话，能力判定仍会按 kind=mobile 拒绝工具。
	if h.currentTokenKind(r) == tokenKindWebview {
		req.ProjectID = ""
		req.WorkspacePath = ""
		req.AccessMode = "confirm"
	}
	sess, err := h.d.Sessions.Create(r.Context(), service.CreateSessionInput{
		Title:         req.Title,
		ModelRef:      req.ModelRef,
		WorkspacePath: req.WorkspacePath,
		ProjectID:     req.ProjectID,
		WorkspaceID:   req.WorkspaceID,
		AccessMode:    req.AccessMode,
	})
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
// access_mode 非空时更新访问模式（confirm/full，切换即时生效）；
// project_id 用指针区分「未传」与「显式移出项目（空串）」。
type updateSessionReq struct {
	Title      string  `json:"title"`
	ModelRef   *string `json:"model_ref"`
	AccessMode string  `json:"access_mode"`
	ProjectID  *string `json:"project_id"`
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
	if req.ProjectID != nil {
		if err := h.d.Sessions.SetProject(r.Context(), r.PathValue("sid"), *req.ProjectID); err != nil {
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

// activeRun 查询会话当前进行中的 Run：GET /api/v1/sessions/{sid}/active-run
//
// 前端刷新（F5）后本地流式态归零，靠本端点重建 SSE 订阅与「进行中」UI。
// 语义铁律：**无进行中 Run 不是 404** —— 返回 200 + run_id:""，前端无需分支处理。
// 刻意不放在 /runs/ 下，避免与 GET /runs/{rid} 的 ServeMux 模式优先级产生歧义
// （"active-run" 会被 {rid} 通配吃掉）。
func (h *handlers) activeRun(w http.ResponseWriter, r *http.Request) {
	run, err := h.d.Runs.ActiveRun(r.Context(), r.PathValue("sid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	resp := map[string]any{"run_id": "", "status": ""}
	if run != nil {
		resp["run_id"] = run.ID
		resp["status"] = string(run.Status)
	}
	writeJSON(w, http.StatusOK, resp)
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
