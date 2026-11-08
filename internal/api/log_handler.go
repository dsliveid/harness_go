package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/logvault"
	"harness-go/internal/service"
)

// 本文件承载「日志与审计查询」的全部端点。
//
// 分层（对应 docs/日志与审计查询方案.md 的三层模型）：
//   - /logs            文本运维日志（app.log 轮转族，人看：启动/panic/访问日志）
//   - /audit/tool-events  结构化审计（天库副本，机器查：跨会话按时间检索工具调用）
//   - /audit/llm-calls    LLM 调用流水（天库副本，跨会话；session_id 可选过滤）
//     · /sessions/{sid}/llm-trace 为其兼容别名，指向同一 handler
//   - /stats/tokens    用量聚合（rollup 预聚合，30 天 <20ms）
//   - /logs/config     抓取档位、保留天数与日志级别（界面运行时开关）
//
// 权限：全部要求 admin scope —— 日志含他人会话内容、IP 与错误堆栈。

// ---------- GET /api/v1/logs ----------

// logsResponse 日志查询响应。
//
// 关键：retention_days / earliest_available_day / snapshot_mode 三个字段是给界面
// 「明示保留策略」用的。用户选了更早的时间范围时，界面能明确告知「该时段日志已清理」
// 而不是显示空白让人以为没有日志。
type logsResponse struct {
	Items []logLine `json:"items"`
	// Total 命中行数（倒序早停时为下界）
	Total int `json:"total"`
	// HasMore 是否还有更多
	HasMore bool `json:"has_more"`
	// RetentionDays 在线保留天数（来自配置/settings）
	RetentionDays int `json:"retention_days"`
	// EarliestAvailableDay 最早可查天键 YYYYMMDD（空 = 无任何日志）
	EarliestAvailableDay string `json:"earliest_available_day"`
	// SnapshotMode 当前报文抓取档位 off|preview|full
	SnapshotMode string `json:"snapshot_mode"`
}

type logLine struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

func (h *handlers) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	res, err := h.d.Logs.Query(r.Context(), service_LogQuery(q))
	if err != nil {
		writeError(w, r, err)
		return
	}
	resp := logsResponse{
		Items:   make([]logLine, 0, len(res.Items)),
		Total:   res.Total,
		HasMore: res.HasMore,
	}
	for _, it := range res.Items {
		resp.Items = append(resp.Items, logLine{Level: it.Level, Text: it.Text})
	}
	// 保留策略元信息（界面顶部说明条的数据源，禁止前端硬编码）
	if h.d.LogConfig != nil {
		resp.RetentionDays = h.d.LogConfig.RetentionDays(r.Context())
		resp.SnapshotMode = h.d.LogConfig.SnapshotMode(r.Context())
	}
	if h.d.Vault != nil {
		resp.EarliestAvailableDay, _ = h.d.Vault.AvailableRange()
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------- GET /api/v1/logs/config & PATCH ----------

type logConfigResponse struct {
	SnapshotMode  string `json:"snapshot_mode"`
	RetentionDays int    `json:"retention_days"`
	// Level 当前进程内**真实生效**的日志级别（debug|info|warn|error）。
	// 来自 slog.LevelVar 而非仅读库：界面显示必须与运行时一致，否则会骗人。
	Level string `json:"level"`
	// EstimatedDailyMB 各档位的预估日增（MB），供界面量化提示
	EstimatedDailyMB map[string]int `json:"estimated_daily_mb"`
	// EarliestAvailableDay / LatestAvailableDay 可查范围
	EarliestAvailableDay string `json:"earliest_available_day"`
	LatestAvailableDay   string `json:"latest_available_day"`
	// TotalBytes 当前日志库族总占用（界面提示磁盘影响）
	TotalBytes int64 `json:"total_bytes"`
	// Dropped / Written 写入管道累计计数（诊断用：dropped > 0 说明队列打满过）
	Dropped int64 `json:"dropped"`
	Written int64 `json:"written"`
}

func (h *handlers) getLogConfig(w http.ResponseWriter, r *http.Request) {
	resp := logConfigResponse{
		// 预估日增：来自方案测算（10 亿 token/天量级）。
		// 这三个数字是给用户判断「切到 full 会不会撑爆磁盘」的依据，必须如实标注。
		EstimatedDailyMB: map[string]int{
			"off":     50,
			"preview": 500,
			"full":    5000,
		},
	}
	if h.d.LogConfig != nil {
		resp.SnapshotMode = h.d.LogConfig.SnapshotMode(r.Context())
		resp.RetentionDays = h.d.LogConfig.RetentionDays(r.Context())
		resp.Level = h.d.LogConfig.Level(r.Context())
	}
	if h.d.Vault != nil {
		resp.EarliestAvailableDay, resp.LatestAvailableDay = h.d.Vault.AvailableRange()
		resp.TotalBytes = h.d.Vault.TotalSizeBytes()
	}
	if h.d.LogWriter != nil {
		resp.Dropped = h.d.LogWriter.Dropped()
		resp.Written = h.d.LogWriter.Written()
	}
	writeJSON(w, http.StatusOK, resp)
}

type logConfigPatch struct {
	SnapshotMode  *string `json:"snapshot_mode,omitempty"`
	RetentionDays *int    `json:"retention_days,omitempty"`
	// Level 日志级别热调（debug|info|warn|error）。写入后进程内 slog.LevelVar
	// 立即生效，控制台与 data/app.log 同步变化，无需重启。
	Level *string `json:"level,omitempty"`
}

func (h *handlers) patchLogConfig(w http.ResponseWriter, r *http.Request) {
	if h.d.LogConfig == nil {
		writeError(w, r, unavailable("日志配置服务未接入"))
		return
	}
	var req logConfigPatch
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if req.SnapshotMode != nil {
		if _, err := h.d.LogConfig.SetSnapshotMode(r.Context(), *req.SnapshotMode); err != nil {
			writeError(w, r, err)
			return
		}
	}
	if req.RetentionDays != nil {
		if _, err := h.d.LogConfig.SetRetentionDays(r.Context(), *req.RetentionDays); err != nil {
			writeError(w, r, err)
			return
		}
	}
	if req.Level != nil {
		if _, err := h.d.LogConfig.SetLevel(r.Context(), *req.Level); err != nil {
			writeError(w, r, err)
			return
		}
	}
	h.getLogConfig(w, r)
}

// ---------- GET /api/v1/audit/tool-events ----------

type auditToolEventsResponse struct {
	Items        []logvault.ToolRow `json:"items"`
	Total        int                `json:"total"`
	RetentionDays int               `json:"retention_days"`
	EarliestAvailableDay string     `json:"earliest_available_day"`
}

func (h *handlers) auditToolEvents(w http.ResponseWriter, r *http.Request) {
	if h.d.Vault == nil {
		writeError(w, r, unavailable("审计日志未接入"))
		return
	}
	q := r.URL.Query()
	from, to, err := parseRange(q.Get("from"), q.Get("to"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	rows, total, err := h.d.Vault.QueryTools(r.Context(), logvault.ToolQuery{
		From:      from,
		To:        to,
		SessionID: q.Get("session_id"),
		RunID:     q.Get("run_id"),
		ToolName:  q.Get("tool"),
		Status:    q.Get("status"),
		Keyword:   q.Get("keyword"),
		Limit:     intParam(r, "limit", 100),
		Offset:    intParam(r, "offset", 0),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	resp := auditToolEventsResponse{Items: rows, Total: total}
	if h.d.LogConfig != nil {
		resp.RetentionDays = h.d.LogConfig.RetentionDays(r.Context())
	}
	resp.EarliestAvailableDay, _ = h.d.Vault.AvailableRange()
	writeJSON(w, http.StatusOK, resp)
}

// ---------- GET /api/v1/audit/llm-calls（兼 /sessions/{sid}/llm-trace 别名） ----------

// auditLLMCallsResponse LLM 调用流水响应。
type auditLLMCallsResponse struct {
	Items []logvault.LLMRow `json:"items"`
	Total int               `json:"total"`
	// Payload 单条回查时的正文（full 档）；空串表示当前档位未保存正文
	Payload string `json:"payload,omitempty"`
	// PayloadHint 正文不可得时的人话说明
	PayloadHint  string `json:"payload_hint,omitempty"`
	SnapshotMode string `json:"snapshot_mode"`
	// RetentionDays / EarliestAvailableDay 保留策略元信息（界面顶部说明条数据源）
	RetentionDays        int    `json:"retention_days"`
	EarliestAvailableDay string `json:"earliest_available_day"`
}

// auditLLMCalls 查询 LLM 调用流水。
//
// 【为什么只有一个 handler 两个路由】
// 历史形态是「工具审计全局、报文查询绑死单会话」的不对称双轨：
// /audit/tool-events 可跨会话，而 /sessions/{sid}/llm-trace 的 {sid} 是必填路径参数。
// 桌面端的对话日志面板需要的是「先看全局流水，再点进某次调用」，
// 于是本次把两者合并为同一查询模型：session_id 变成**可选过滤条件**，
// logvault.LLMQuery.SessionID 为空本就表示不过滤，能力早已具备。
//
// 结果：一份查询逻辑、两个路由入口（旧路径作为兼容别名注入 sid），零重复代码。
// 工具与报文两条审计线从此同构（都是跨会话 + 可选 session 过滤）。
func (h *handlers) auditLLMCalls(w http.ResponseWriter, r *http.Request) {
	if h.d.Vault == nil {
		writeError(w, r, unavailable("审计日志未接入"))
		return
	}
	q := r.URL.Query()
	// 兼容别名：/sessions/{sid}/llm-trace 的路径参数优先于 query（路径更具体、更不易误传）
	sid := r.PathValue("sid")
	if sid == "" {
		sid = q.Get("session_id")
	}
	from, to, err := parseRange(q.Get("from"), q.Get("to"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	rows, total, err := h.d.Vault.QueryLLM(r.Context(), logvault.LLMQuery{
		From:      from,
		To:        to,
		SessionID: sid,
		RunID:     q.Get("run_id"),
		MessageID: q.Get("message_id"),
		TraceID:   q.Get("trace_id"),
		Model:     q.Get("model"),
		Status:    q.Get("status"),
		Keyword:   q.Get("keyword"),
		Limit:     intParam(r, "limit", 100),
		Offset:    intParam(r, "offset", 0),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	resp := auditLLMCallsResponse{Items: rows, Total: total}
	if h.d.LogConfig != nil {
		resp.SnapshotMode = h.d.LogConfig.SnapshotMode(r.Context())
		resp.RetentionDays = h.d.LogConfig.RetentionDays(r.Context())
	}
	resp.EarliestAvailableDay, _ = h.d.Vault.AvailableRange()
	// 单条精确回查时顺带取正文（列表不取，避免 N 次 blob 读）
	if id := q.Get("id"); id != "" && len(rows) > 0 {
		payload, pErr := h.d.Vault.GetPayload(r.Context(), id, rows[0].CreatedAt)
		if pErr != nil {
			writeError(w, r, pErr)
			return
		}
		resp.Payload = payload
		if payload == "" {
			// 诚实降级：不说「无数据」，说清为什么没有
			switch resp.SnapshotMode {
			case "preview":
				resp.PayloadHint = "当前为「预览」档：只保存了脱敏摘要与校验值，不保存正文。切到「完整」档后新调用才会有正文（不补录历史）。"
			case "off":
				resp.PayloadHint = "当前为「关闭」档：只记录元数据，不保存任何报文内容。"
			default:
				resp.PayloadHint = "正文已随保留期清理或外置文件缺失。"
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------- GET /api/v1/stats/tokens ----------

func (h *handlers) tokenStats(w http.ResponseWriter, r *http.Request) {
	if h.d.Vault == nil {
		writeError(w, r, unavailable("审计日志未接入"))
		return
	}
	q := r.URL.Query()
	from, to, err := parseRange(q.Get("from"), q.Get("to"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	sum, err := h.d.Vault.AggregateTokens(r.Context(), logvault.TokenQuery{
		From:    from,
		To:      to,
		GroupBy: logvault.TokenGroup(strings.TrimSpace(q.Get("group_by"))),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// ---------- 辅助 ----------

// parseRange 解析时间范围参数（Unix 毫秒；缺省 = 最近 24 小时）。
//
// 为什么必须有默认范围：不传 from/to 时若默认全量扫描，30 天天库要全开一遍，
// 在目标量级下就是秒级延迟。默认最近 24h 让「打开页面」始终是快的。
func parseRange(fromStr, toStr string) (int64, int64, error) {
	const dayMs = 24 * 3600 * 1000
	var from, to int64
	if fromStr != "" {
		v, err := strconv.ParseInt(fromStr, 10, 64)
		if err != nil || v <= 0 {
			return 0, 0, badRequest("from 必须是 Unix 毫秒时间戳")
		}
		from = v
	}
	if toStr != "" {
		v, err := strconv.ParseInt(toStr, 10, 64)
		if err != nil || v <= 0 {
			return 0, 0, badRequest("to 必须是 Unix 毫秒时间戳")
		}
		to = v
	}
	now := nowMs()
	if to == 0 {
		to = now
	}
	if from == 0 {
		from = to - dayMs
	}
	if to < from {
		return 0, 0, badRequest("to 不能早于 from")
	}
	return from, to, nil
}

// service_LogQuery 把 HTTP query 映射为服务层查询条件。
func service_LogQuery(q map[string][]string) service.LogQuery {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	return service.LogQuery{
		Limit:   atoiDefault(get("limit"), 200),
		Offset:  atoiDefault(get("offset"), 0),
		Level:   get("level"),
		TraceID: get("traceId"),
		Keyword: get("keyword"),
		From:    atoiDefault64(get("from")),
		To:      atoiDefault64(get("to")),
	}
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func atoiDefault64(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// nowMs 当前 Unix 毫秒（抽出便于测试注入）。
func nowMs() int64 { return time.Now().UnixMilli() }

// unavailable 子系统未接线时的统一错误（nil 依赖降级为 503 而非 panic）。
func unavailable(msg string) error {
	return apperr.New(apperr.CodeUnavailable, "%s", msg)
}

// badRequest 参数校验失败（400）。
func badRequest(msg string) error {
	return apperr.New(apperr.CodeInvalidArgument, "%s", msg)
}
