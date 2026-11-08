package logvault

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ============ 查询模型 ============

// LLMQuery LLM 报文日志查询条件（时间范围必填，其余可选）。
//
// 所有条件最终都以占位符绑定，**绝不字符串拼接用户输入**
// （where 片段只由本文件内的常量组成）。
type LLMQuery struct {
	From        int64  // 必填（Unix 毫秒）
	To          int64  // 必填
	SessionID   string
	RunID       string
	MessageID   string
	TraceID     string
	Model       string
	Status      string
	Keyword     string // 在 payload_preview 上做子串匹配（限长保护）
	Limit       int    // 默认 100，上限 1000
	Offset      int    // 深分页建议改用游标（见 NextFrom）
}

// ToolQuery 工具调用日志查询条件。
type ToolQuery struct {
	From      int64
	To        int64
	SessionID string
	RunID     string
	ToolName  string
	Status    string
	Keyword   string
	Limit     int
	Offset    int
}

// LLMRow 查询结果行。
type LLMRow struct {
	ID               string  `json:"id"`
	CreatedAt        int64   `json:"created_at"`
	SessionID        string  `json:"session_id"`
	RunID            string  `json:"run_id"`
	MessageID        string  `json:"message_id"`
	TraceID          string  `json:"trace_id"`
	Model            string  `json:"model"`
	Provider         string  `json:"provider"`
	Endpoint         string  `json:"endpoint"`
	PayloadPreview   string  `json:"payload_preview"`
	PayloadInline    string  `json:"payload_inline,omitempty"`
	PayloadSHA256    string  `json:"payload_sha256"`
	PayloadBytes     int64   `json:"payload_bytes"`
	PayloadAvailable bool    `json:"payload_available"` // 正文是否可取（inline 或 blob 存在）
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	IsEstimated      bool    `json:"is_estimated"`
	TTFTMs           int64   `json:"ttft_ms"`
	DurationMs       int64   `json:"duration_ms"`
	TokensPerSec     float64 `json:"tokens_per_sec"`
	Status           string  `json:"status"`
	ErrorCode        string  `json:"error_code,omitempty"`
	ErrorMessage     string  `json:"error_message,omitempty"`
}

// ToolRow 工具调用日志结果行。
type ToolRow struct {
	ID            string `json:"id"`
	CreatedAt     int64  `json:"created_at"`
	SessionID     string `json:"session_id"`
	RunID         string `json:"run_id"`
	ToolEventID   string `json:"tool_event_id"`
	ToolName      string `json:"tool_name"`
	Namespace     string `json:"namespace"`
	ParamsPreview string `json:"params_preview"`
	ResultPreview string `json:"result_preview"`
	Bytes         int64  `json:"bytes"`
	Status        string `json:"status"`
	Risk          string `json:"risk"`
	Error         string `json:"error,omitempty"`
	DurationMs    int64  `json:"duration_ms"`
}

// Querier 天库查询器（跨天自动路由）。
type Querier struct {
	shard *Shard
	blobs *BlobStore
}

// NewQuerier 创建查询器。
func NewQuerier(shard *Shard, blobs *BlobStore) *Querier {
	return &Querier{shard: shard, blobs: blobs}
}

// Shard 暴露底层天分片管理器（上层需要 AvailableRange / TotalSizeBytes 明示保留边界）。
func (q *Querier) Shard() *Shard { return q.shard }

// AvailableRange 转发到 Shard（最早/最晚可查天键）。
func (q *Querier) AvailableRange() (earliest, latest string) {
	if q == nil || q.shard == nil {
		return "", ""
	}
	return q.shard.AvailableRange()
}

// TotalSizeBytes 转发到 Shard（日志库族总占用）。
func (q *Querier) TotalSizeBytes() int64 {
	if q == nil || q.shard == nil {
		return 0
	}
	return q.shard.TotalSizeBytes()
}

// maxKeywordLen 关键字长度上限（防止超长 LIKE 拖垮查询）。
const maxKeywordLen = 128

// maxLimit 单次查询条数上限。
const maxLimit = 1000

// effectiveLimit 归一化 limit（默认 100，上限 maxLimit）。
//
// 抽成方法而非局部变量，是因为「SQL 侧 LIMIT 下推」与「上层分页」两处
// 必须用同一个归一化口径，否则下推的条数可能少于实际需要的，导致漏数据。
func (q LLMQuery) effectiveLimit() int {
	if q.Limit <= 0 {
		return 100
	}
	if q.Limit > maxLimit {
		return maxLimit
	}
	return q.Limit
}

// daysInRange 返回 [from,to] 覆盖的天键列表（升序）。
func daysInRange(from, to int64) []string {
	if to < from {
		return nil
	}
	start := time.UnixMilli(from)
	end := time.UnixMilli(to)
	var out []string
	for d := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.Local); !d.After(end); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format(dayLayout))
		// 防御：极端范围（>10 年）直接截断，避免无限循环
		if len(out) >= 3660 {
			break
		}
	}
	return out
}

// QueryLLM 跨天查询 LLM 报文日志。
//
// 跨天策略：只打开 [from,to] 覆盖的天库（通常 1~2 个），逐天查询后按 created_at
// 合并排序，再应用 limit/offset。**不遍历全部天库**。
func (q *Querier) QueryLLM(ctx context.Context, qq LLMQuery) ([]LLMRow, int, error) {
	if qq.To < qq.From {
		return nil, 0, fmt.Errorf("logvault: to 不能早于 from")
	}
	limit := qq.effectiveLimit()
	if len(qq.Keyword) > maxKeywordLen {
		qq.Keyword = qq.Keyword[:maxKeywordLen]
	}

	var all []LLMRow
	for _, day := range daysInRange(qq.From, qq.To) {
		rows, err := q.queryLLMDay(ctx, day, qq)
		if err != nil {
			// 单天失败不阻断整体（该天库可能已被清理或损坏）
			continue
		}
		all = append(all, rows...)
	}
	// 跨天合并后按时间倒序（新 → 旧）
	sortRowsByCreatedDesc(all)
	total := len(all)
	// 分页
	if qq.Offset >= total {
		return []LLMRow{}, total, nil
	}
	end := qq.Offset + limit
	if end > total {
		end = total
	}
	return all[qq.Offset:end], total, nil
}

// queryLLMDay 查询单个天库。
func (q *Querier) queryLLMDay(ctx context.Context, day string, qq LLMQuery) ([]LLMRow, error) {
	from, to := qq.From, qq.To
	ds, err := DayStartMs(day)
	if err != nil {
		return nil, err
	}
	de, err := DayEndMs(day)
	if err != nil {
		return nil, err
	}
	if from < ds {
		from = ds
	}
	if to > de {
		to = de
	}
	if from > de || to < ds {
		return nil, nil
	}

	where := []string{"created_at BETWEEN ? AND ?"}
	args := []any{from, to}
	if qq.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, qq.SessionID)
	}
	if qq.RunID != "" {
		where = append(where, "run_id = ?")
		args = append(args, qq.RunID)
	}
	if qq.MessageID != "" {
		where = append(where, "message_id = ?")
		args = append(args, qq.MessageID)
	}
	if qq.TraceID != "" {
		where = append(where, "trace_id = ?")
		args = append(args, qq.TraceID)
	}
	if qq.Model != "" {
		where = append(where, "model = ?")
		args = append(args, qq.Model)
	}
	if qq.Status != "" {
		where = append(where, "status = ?")
		args = append(args, qq.Status)
	}
	if qq.Keyword != "" {
		where = append(where, "payload_preview LIKE ? ESCAPE '\\'")
		args = append(args, "%"+escapeLike(qq.Keyword)+"%")
	}

	sqlStr := `SELECT id, created_at, session_id, run_id, message_id, trace_id, model, provider,
		endpoint, payload_preview, payload_inline, payload_sha256, payload_bytes,
		prompt_tokens, completion_tokens, cached_tokens, is_estimated,
		ttft_ms, duration_ms, tokens_per_sec, status, error_code, error_message
		FROM llm_requests WHERE ` + strings.Join(where, " AND ") + ` ORDER BY created_at DESC`

	db, err := q.shard.Day(ctx, day)
	if err != nil {
		// 天库不存在（该天无数据或已清理）→ 返回空
		return nil, nil
	}
	// ⚠️ 性能关键：必须在 SQL 侧下推 LIMIT，否则「查最近 100 条」会把该天全部命中行
	// （实测 5 万行规模下 2000+ 行 × 23 列）都拉进内存再排序，单次查询达 237ms。
	//
	// 下推的正确性：跨天合并需要「每生前 offset+limit 条」才可能拼出全局正确结果，
	// 因为单天贡献的行数不超过它的前 (offset+limit) 条。故按 perDay = offset+limit 取。
	perDay := qq.Offset + qq.effectiveLimit()
	if perDay <= 0 {
		perDay = qq.effectiveLimit()
	}
	sqlStr += fmt.Sprintf(" LIMIT %d", perDay)
	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LLMRow
	for rows.Next() {
		var (
			r                                                LLMRow
			preview, inline, sha, errCode, errMsg            sql.NullString
			estimated                                        int
		)
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.SessionID, &r.RunID, &r.MessageID, &r.TraceID,
			&r.Model, &r.Provider, &r.Endpoint, &preview, &inline, &sha, &r.PayloadBytes,
			&r.PromptTokens, &r.CompletionTokens, &r.CachedTokens, &estimated,
			&r.TTFTMs, &r.DurationMs, &r.TokensPerSec, &r.Status, &errCode, &errMsg); err != nil {
			return nil, err
		}
		r.PayloadPreview = preview.String
		r.PayloadSHA256 = sha.String
		r.PayloadInline = inline.String
		r.IsEstimated = estimated != 0
		r.ErrorCode = errCode.String
		r.ErrorMessage = errMsg.String
		r.PayloadAvailable = inline.Valid || (sha.Valid && q.blobExists(day, sha.String))
		out = append(out, r)
	}
	return out, rows.Err()
}

// blobExists 判断正文 blob 是否存在（preview 档不存在属正常）。
func (q *Querier) blobExists(day, hash string) bool {
	if q.blobs == nil || hash == "" {
		return false
	}
	return q.blobs.Exists(day, hash)
}

// QueryTools 跨天查询工具调用日志。
func (q *Querier) QueryTools(ctx context.Context, tq ToolQuery) ([]ToolRow, int, error) {
	if tq.To < tq.From {
		return nil, 0, fmt.Errorf("logvault: to 不能早于 from")
	}
	limit := tq.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	if len(tq.Keyword) > maxKeywordLen {
		tq.Keyword = tq.Keyword[:maxKeywordLen]
	}
	var all []ToolRow
	for _, day := range daysInRange(tq.From, tq.To) {
		rows, err := q.queryToolsDay(ctx, day, tq)
		if err != nil {
			continue
		}
		all = append(all, rows...)
	}
	sortToolsDesc(all)
	total := len(all)
	if tq.Offset >= total {
		return []ToolRow{}, total, nil
	}
	end := tq.Offset + limit
	if end > total {
		end = total
	}
	return all[tq.Offset:end], total, nil
}

func (q *Querier) queryToolsDay(ctx context.Context, day string, tq ToolQuery) ([]ToolRow, error) {
	ds, err := DayStartMs(day)
	if err != nil {
		return nil, err
	}
	de, err := DayEndMs(day)
	if err != nil {
		return nil, err
	}
	from, to := tq.From, tq.To
	if from < ds {
		from = ds
	}
	if to > de {
		to = de
	}
	if from > de || to < ds {
		return nil, nil
	}
	where := []string{"created_at BETWEEN ? AND ?"}
	args := []any{from, to}
	if tq.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, tq.SessionID)
	}
	if tq.RunID != "" {
		where = append(where, "run_id = ?")
		args = append(args, tq.RunID)
	}
	if tq.ToolName != "" {
		where = append(where, "tool_name = ?")
		args = append(args, tq.ToolName)
	}
	if tq.Status != "" {
		where = append(where, "status = ?")
		args = append(args, tq.Status)
	}
	if tq.Keyword != "" {
		where = append(where, "(params_preview LIKE ? ESCAPE '\\' OR result_preview LIKE ? ESCAPE '\\')")
		kw := "%" + escapeLike(tq.Keyword) + "%"
		args = append(args, kw, kw)
	}
	sqlStr := `SELECT id, created_at, session_id, run_id, tool_event_id, tool_name, namespace,
		params_preview, result_preview, bytes, status, risk, error, duration_ms
		FROM tool_call_logs WHERE ` + strings.Join(where, " AND ") + ` ORDER BY created_at DESC`
	db, err := q.shard.Day(ctx, day)
	if err != nil {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ToolRow
	for rows.Next() {
		var (
			r                          ToolRow
			params, result, errMsg     sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.SessionID, &r.RunID, &r.ToolEventID,
			&r.ToolName, &r.Namespace, &params, &result, &r.Bytes, &r.Status, &r.Risk,
			&errMsg, &r.DurationMs); err != nil {
			return nil, err
		}
		r.ParamsPreview = params.String
		r.ResultPreview = result.String
		r.Error = errMsg.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetPayload 取某条 LLM 记录的正文全文。
//
// 优先级：payload_inline（full 档 ≤8KB）→ blob（full 档 >8KB）→ 空（preview/off 档）。
// 返回空正文时上层应提示「当前档位未保存正文」，而不是报错。
func (q *Querier) GetPayload(ctx context.Context, id string, createdAt int64) (string, error) {
	day := DayOf(createdAt)
	db, err := q.shard.Day(ctx, day)
	if err != nil {
		return "", nil
	}
	var inline, sha sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT payload_inline, payload_sha256 FROM llm_requests WHERE id=?`, id).
		Scan(&inline, &sha)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	if inline.Valid && inline.String != "" {
		return inline.String, nil
	}
	if sha.Valid && sha.String != "" && q.blobs != nil {
		data, bErr := q.blobs.Get(day, sha.String)
		if bErr != nil {
			return "", bErr
		}
		return string(data), nil
	}
	return "", nil
}

// escapeLike 转义 LIKE 通配符（配合 ESCAPE '\\'）。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// sortRowsByCreatedDesc 按创建时间倒序（同时间按 ID 稳定）。
func sortRowsByCreatedDesc(rows []LLMRow) {
	if len(rows) < 2 {
		return
	}
	// 插入排序在「单天已近乎有序」的场景下比 sort.Slice 更省分配
	for i := 1; i < len(rows); i++ {
		cur := rows[i]
		j := i - 1
		for j >= 0 && (rows[j].CreatedAt < cur.CreatedAt ||
			(rows[j].CreatedAt == cur.CreatedAt && rows[j].ID < cur.ID)) {
			rows[j+1] = rows[j]
			j--
		}
		rows[j+1] = cur
	}
}

func sortToolsDesc(rows []ToolRow) {
	if len(rows) < 2 {
		return
	}
	for i := 1; i < len(rows); i++ {
		cur := rows[i]
		j := i - 1
		for j >= 0 && (rows[j].CreatedAt < cur.CreatedAt ||
			(rows[j].CreatedAt == cur.CreatedAt && rows[j].ID < cur.ID)) {
			rows[j+1] = rows[j]
			j--
		}
		rows[j+1] = cur
	}
}
