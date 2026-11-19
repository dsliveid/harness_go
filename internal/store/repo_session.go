package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"harness-go/internal/model"
)

// ============ 消息 ============

// MessageRepo 会话库消息仓储（seq 在会话内单调递增且唯一）。
type MessageRepo struct{ s *SessionDB }

// Messages 返回消息仓储。
func (s *SessionDB) Messages() *MessageRepo { return &MessageRepo{s: s} }

// Append 追加消息并分配 seq（事务内 MAX(seq)+1，保证会话内唯一）。
//
// 注：seq 是会话内唯一，不再需要 session_id 复合唯一（分库后一个库只有一个会话）。
func (r *MessageRepo) Append(ctx context.Context, m *model.Message) error {
	if m.ID == "" {
		return errors.New("store: 消息 ID 不能为空")
	}
	if m.Role == "" {
		return errors.New("store: 消息 role 不能为空")
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = NowMillis()
	}
	return r.s.withTx(ctx, func(tx *sql.Tx) error {
		var maxSeq sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM messages`).Scan(&maxSeq); err != nil {
			return fmt.Errorf("store: 读取最大 seq 失败: %w", err)
		}
		m.Seq = maxSeq.Int64 + 1
		_, err := tx.ExecContext(ctx,
			`INSERT INTO messages(id, seq, run_id, role, content, reasoning,
			                      tool_calls_json, tool_call_id, attachments_json,
			                      token_count, usage_json, created_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			m.ID, m.Seq, nullIfEmpty(m.RunID), string(m.Role), m.Content,
			nullIfEmpty(m.Reasoning), rawOrNil(m.ToolCallsJSON), nullIfEmpty(m.ToolCallID),
			rawOrNil(m.AttachmentsJSON), m.TokenCount, rawOrNil(m.UsageJSON), m.CreatedAt)
		if err != nil {
			return fmt.Errorf("store: 追加消息失败: %w", err)
		}
		return nil
	})
}

const messageColumns = `id, seq, run_id, role, content, reasoning, tool_calls_json,
	tool_call_id, attachments_json, token_count, usage_json, created_at, hidden`

func scanMessage(sc interface{ Scan(...any) error }) (model.Message, error) {
	var (
		m        model.Message
		runID    sql.NullString
		reason   sql.NullString
		toolCall sql.NullString
		toolID   sql.NullString
		attach   sql.NullString
		usage    sql.NullString
		hidden   int
	)
	err := sc.Scan(&m.ID, &m.Seq, &runID, &m.Role, &m.Content, &reason,
		&toolCall, &toolID, &attach, &m.TokenCount, &usage, &m.CreatedAt, &hidden)
	if err != nil {
		return model.Message{}, err
	}
	if runID.Valid {
		m.RunID = runID.String
	}
	if reason.Valid {
		m.Reasoning = reason.String
	}
	if toolCall.Valid {
		m.ToolCallsJSON = json.RawMessage(toolCall.String)
	}
	if toolID.Valid {
		m.ToolCallID = toolID.String
	}
	if attach.Valid {
		m.AttachmentsJSON = json.RawMessage(attach.String)
	}
	if usage.Valid {
		m.UsageJSON = json.RawMessage(usage.String)
	}
	m.Hidden = hidden != 0
	return m, nil
}

// List 按 seq 升序分页读取（afterSeq=0 表示从头）。
func (r *MessageRepo) List(ctx context.Context, afterSeq int64, limit int) ([]model.Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE seq > ? ORDER BY seq ASC LIMIT ?`,
		afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 读取消息失败: %w", err)
	}
	defer rows.Close()
	out := make([]model.Message, 0, limit)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 解析消息行失败: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// maxTailLimit ListTail / ListVisibleTail 的硬上限。
//
// 与 List 的 500 上限不同：这两个函数是**引擎内部**（单会话、单人）读全量上下文
// 的唯一入口，物理上最多也就几千条；给 5000 是为了既能装下超长会话，又不会被
// 异常参数一次拉爆内存。
const maxTailLimit = 5000

// ListTail 按 seq **倒序**取最近 limit 条后反转成升序返回（引擎组装上下文专用）。
//
// # 为什么必须新增，而不是把 List 的 limit 调大
//
// 真实事故（会话 122559a3，258 条消息 / 839 万 token 账单）：引擎两处调用点
// 都传 `ListMessages(ctx, 0, 1000)`，而 `List` 内部有防洪闸：
//
//	if limit <= 0 || limit > 500 { limit = 100 }
//
// 于是 `1000` 被**静默改写为 100**，且 SQL 是 `ORDER BY seq ASC`（取头不取尾）——
// 引擎拿到的是**最旧的 100 条**。后果有两个，都很致命：
//
//	① 跨 Run 续聊（第 2 个 Run 从 seq42 开始）时，最近几十步执行进展全部丢失，
//	   模型明明"刚做完"却对自己做过什么一无所知；
//	② tryCompact 也只看到最旧 100 条 → compact.PlanCompaction 找不到「已完成轮次」
//	   （lastUser 恒为 0）→ 压缩永不发生 → 单会话烧到 839 万 token。
//
// 那条防洪闸本身是**对的**（List 是 HTTP 分页接口的底座，不能因外部参数打爆内存），
// 所以：**不要改 List，改用这个新入口**。语义差异必须显式体现在函数名上
// （Tail = 取最新），而不是靠调用方记得传对参数。
func (r *MessageRepo) ListTail(ctx context.Context, limit int) ([]model.Message, error) {
	msgs, err := r.listTail(ctx, limit, false)
	return msgs, err
}

// ListVisibleTail 同 ListTail，但只返回未置灰（hidden=0）消息。
//
// 引擎侧必须用这个：被回滚置灰的消息不得进入模型上下文（否则回滚形同虚设）。
func (r *MessageRepo) ListVisibleTail(ctx context.Context, limit int) ([]model.Message, error) {
	return r.listTail(ctx, limit, true)
}

func (r *MessageRepo) listTail(ctx context.Context, limit int, visibleOnly bool) ([]model.Message, error) {
	if limit <= 0 || limit > maxTailLimit {
		limit = maxTailLimit
	}
	q := `SELECT ` + messageColumns + ` FROM messages`
	if visibleOnly {
		q += ` WHERE hidden = 0`
	}
	q += ` ORDER BY seq DESC LIMIT ?`
	rows, err := r.s.conn().QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 读取尾部消息失败: %w", err)
	}
	defer rows.Close()
	out := make([]model.Message, 0, limit)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 解析消息行失败: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 倒序取回后必须反转：调用方（引擎、压缩规划）全部假定 seq 升序。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ListVisible 只读未置灰消息（阶段 G：引擎组装上下文用；
// 置灰消息保留在库里供审计与 reapply，但不进模型上下文）。
func (r *MessageRepo) ListVisible(ctx context.Context, afterSeq int64, limit int) ([]model.Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE seq > ? AND hidden = 0 ORDER BY seq ASC LIMIT ?`,
		afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 读取可见消息失败: %w", err)
	}
	defer rows.Close()
	out := make([]model.Message, 0, limit)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 解析消息行失败: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetHiddenFrom 把 seq >= from 的消息置灰，返回受影响行数（消息级回滚）。
func (r *MessageRepo) SetHiddenFrom(ctx context.Context, from int64) (int64, error) {
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE messages SET hidden = 1 WHERE seq >= ? AND hidden = 0`, from)
	if err != nil {
		return 0, fmt.Errorf("store: 置灰消息失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ClearHiddenFrom 把 seq >= from 的置灰消息恢复可见（reapply）。
func (r *MessageRepo) ClearHiddenFrom(ctx context.Context, from int64) (int64, error) {
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE messages SET hidden = 0 WHERE seq >= ? AND hidden = 1`, from)
	if err != nil {
		return 0, fmt.Errorf("store: 恢复置灰消息失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Count 会话内消息数。
func (r *MessageRepo) Count(ctx context.Context) (int64, error) {
	var n int64
	if err := r.s.conn().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: 统计消息数失败: %w", err)
	}
	return n, nil
}

// UpdateTokenCount 写入精确 token 计数缓存（避免每次重算全量历史）。
func (r *MessageRepo) UpdateTokenCount(ctx context.Context, id string, tokens int) error {
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE messages SET token_count=? WHERE id=?`, tokens, id)
	if err != nil {
		return fmt.Errorf("store: 更新消息 token 数失败: %w", err)
	}
	return ensureAffected(res, "消息 "+id)
}

// ============ 运行 ============

// RunRepo 会话库运行仓储。
type RunRepo struct{ s *SessionDB }

// Runs 返回运行仓储。
func (s *SessionDB) Runs() *RunRepo { return &RunRepo{s: s} }

// Create 创建 Run（默认 pending + executor=server）。
func (r *RunRepo) Create(ctx context.Context, run *model.Run) error {
	if run.ID == "" {
		return errors.New("store: Run ID 不能为空")
	}
	if run.Status == "" {
		run.Status = model.RunPending
	}
	if !run.Status.Valid() {
		return fmt.Errorf("store: 非法 Run 状态: %s", run.Status)
	}
	if run.Executor == "" {
		run.Executor = model.ExecutorServer
	}
	if run.TriggerType == "" {
		run.TriggerType = "manual"
	}
	if run.CreatedAt == 0 {
		run.CreatedAt = NowMillis()
	}
	_, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO runs(id, trigger_type, status, model_ref, executor, device_id,
		                  parent_run_id, created_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		run.ID, run.TriggerType, string(run.Status), run.ModelRef,
		string(run.Executor), nullIfEmpty(run.DeviceID), nullIfEmpty(run.ParentRunID),
		run.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: 创建 Run 失败: %w", err)
	}
	return nil
}

const runColumns = `id, trigger_type, status, stop_reason, model_ref, executor, device_id,
	parent_run_id, input_tokens, output_tokens, cached_tokens, is_estimated,
	error_code, error_message, started_at, ended_at, created_at`

func scanRun(sc interface{ Scan(...any) error }) (model.Run, error) {
	var (
		run     model.Run
		stop    sql.NullString
		device  sql.NullString
		parent  sql.NullString
		errCode sql.NullString
		errMsg  sql.NullString
		started sql.NullInt64
		ended   sql.NullInt64
	)
	err := sc.Scan(&run.ID, &run.TriggerType, &run.Status, &stop, &run.ModelRef,
		&run.Executor, &device, &parent, &run.InputTokens, &run.OutputTokens,
		&run.CachedTokens, &run.IsEstimated, &errCode, &errMsg, &started, &ended, &run.CreatedAt)
	if err != nil {
		return model.Run{}, err
	}
	if stop.Valid {
		run.StopReason = stop.String
	}
	if device.Valid {
		run.DeviceID = device.String
	}
	if parent.Valid {
		run.ParentRunID = parent.String
	}
	if errCode.Valid {
		run.ErrorCode = errCode.String
	}
	if errMsg.Valid {
		run.ErrorMessage = errMsg.String
	}
	if started.Valid {
		v := started.Int64
		run.StartedAt = &v
	}
	if ended.Valid {
		v := ended.Int64
		run.EndedAt = &v
	}
	return run, nil
}

// Get 按 ID 取 Run。
func (r *RunRepo) Get(ctx context.Context, id string) (*model.Run, error) {
	row := r.s.conn().QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM runs WHERE id=?`, id)
	run, err := scanRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: Run %s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("store: 查询 Run 失败: %w", err)
	}
	return &run, nil
}

// Transition 状态迁移（唯一入口，保证状态机单点收口）。
//
// 只允许从"未完成态"迁移到新状态；终态不可再迁移。
//
// 幂等语义（2026-11 修复）：UPDATE 命中 0 行时回查当前状态，若**已是目标状态**则
// 返回 nil。理由：停止链路存在两条并发收口路径 —— StopRun 同步收口 + 引擎 goroutine
// 收尾。谁先落地都合法，后到者不该报 "非法迁移/不存在" 把真实成功伪装成失败
// （调用方据此返回 500，用户看到的却是「停止失败」而实际已停）。
func (r *RunRepo) Transition(ctx context.Context, id string, to model.RunStatus, stopReason, errCode, errMsg string) error {
	if !to.Valid() {
		return fmt.Errorf("store: 非法 Run 状态: %s", to)
	}
	now := NowMillis()
	var startedAt any
	if to == model.RunRunning {
		startedAt = now
	}
	var endedAt any
	if to.IsTerminal() {
		endedAt = now
	}
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE runs SET status=?,
		        stop_reason=COALESCE(?, stop_reason),
		        error_code=COALESCE(?, error_code),
		        error_message=COALESCE(?, error_message),
		        started_at=COALESCE(started_at, ?),
		        ended_at=COALESCE(?, ended_at)
		 WHERE id=? AND status IN ('pending','running','waiting_approval')`,
		string(to), nullIfEmpty(stopReason), nullIfEmpty(errCode), nullIfEmpty(errMsg),
		startedAt, endedAt, id)
	if err != nil {
		return fmt.Errorf("store: Run 状态迁移失败: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// 幂等：已是目标状态视为成功（见函数注释）。
		if cur, gErr := r.Get(ctx, id); gErr == nil && cur.Status == to {
			return nil
		}
	}
	return ensureAffected(res, "Run "+id)
}

// ListUnfinished 列出未完成 Run（启动恢复用）。
// List 全量运行（导出包用；时间正序）。
func (r *RunRepo) List(ctx context.Context) ([]model.Run, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT `+runColumns+` FROM runs ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询 Run 失败: %w", err)
	}
	defer rows.Close()
	var out []model.Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 解析 Run 行失败: %w", err)
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (r *RunRepo) ListUnfinished(ctx context.Context) ([]model.Run, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT `+runColumns+` FROM runs
		 WHERE status IN ('pending','running','waiting_approval') ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询未完成 Run 失败: %w", err)
	}
	defer rows.Close()
	var out []model.Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 解析 Run 行失败: %w", err)
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// AddUsage 累加单次 Run 的 token 用量（P1：引擎终态回写；预埋列的落点）。
//
// is_estimated 与 cached_tokens 是 0001_init.sql:44-45 的预留列，此前从不写入，
// 导致「缓存命中率无分子分母」「估算值无法与真实值区分」。此处一并落库。
// is_estimated 采用 OR 语义累加：只要本 Run 任一轮用量是估算值，整次即标记估算 ——
// 宁可标记为估算，也不把估算值伪装成真实值。
func (r *RunRepo) AddUsage(ctx context.Context, runID string, promptTokens, completionTokens, cachedTokens int64, isEstimated bool) error {
	est := 0
	if isEstimated {
		est = 1
	}
	_, err := r.s.conn().ExecContext(ctx,
		`UPDATE runs SET input_tokens  = input_tokens  + ?,
		                 output_tokens = output_tokens + ?,
		                 cached_tokens = cached_tokens + ?,
		                 is_estimated  = MAX(is_estimated, ?)
		 WHERE id = ?`,
		promptTokens, completionTokens, cachedTokens, est, runID)
	if err != nil {
		return fmt.Errorf("store: 回写 Run 用量失败: %w", err)
	}
	return nil
}

// FailOpenRuns 把未完成 Run 置为 interrupted（进程重启恢复，ADR-009 不做续跑）。
func (r *RunRepo) FailOpenRuns(ctx context.Context, reason string) (int, error) {
	if reason == "" {
		reason = "process_restart"
	}
	now := NowMillis()
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE runs SET status='interrupted', stop_reason=?, ended_at=?
		 WHERE status IN ('pending','running','waiting_approval')`, reason, now)
	if err != nil {
		return 0, fmt.Errorf("store: 标记中断 Run 失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: 读取影响行数失败: %w", err)
	}
	return int(n), nil
}

// ============ 工具事件 ============

// ToolEventRepo 会话库工具事件仓储（审计 + UI 回放）。
type ToolEventRepo struct{ s *SessionDB }

// ToolEvents 返回工具事件仓储。
func (s *SessionDB) ToolEvents() *ToolEventRepo { return &ToolEventRepo{s: s} }

// Insert 记录一次工具调用（pending 态，审批前）。
func (r *ToolEventRepo) Insert(ctx context.Context, ev *model.ToolEvent) error {
	if ev.ID == "" || ev.RunID == "" || ev.ToolName == "" {
		return errors.New("store: 工具事件缺少必要字段")
	}
	if ev.Namespace == "" {
		ev.Namespace = "builtin"
	}
	if ev.Status == "" {
		ev.Status = model.ToolPending
	}
	if ev.Risk == "" {
		ev.Risk = model.RiskReadonly
	}
	if ev.Executor == "" {
		ev.Executor = model.ExecutorServer
	}
	if ev.SourceTrust == "" {
		ev.SourceTrust = model.TrustTrusted
	}
	_, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO tool_events(id, run_id, message_id, tool_name, namespace, args_json,
		                         status, risk, executor, device_id, source_trust, started_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		ev.ID, ev.RunID, nullIfEmpty(ev.MessageID), ev.ToolName, ev.Namespace,
		rawOrNil(ev.ArgsJSON), string(ev.Status), string(ev.Risk), string(ev.Executor),
		nullIfEmpty(ev.DeviceID), string(ev.SourceTrust), ev.StartedAt)
	if err != nil {
		return fmt.Errorf("store: 记录工具事件失败: %w", err)
	}
	return nil
}

// Finish 结束工具调用（写结果、终态与执行面归属）。
//
// exec 为执行面元信息（C-5）：端侧执行落 device/untrusted，静默降级落 server/trusted。
// 空字段按 Normalized 兜底，保证 C 阶段之前的行为不变（server + trusted）。
func (r *ToolEventRepo) Finish(ctx context.Context, id string, status model.ToolEventStatus,
	result json.RawMessage, resultBlob, errMsg string, exec model.ToolEventExec) error {
	e := exec.Normalized()
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE tool_events SET status=?, result_json=?, result_blob=?,
		        error=COALESCE(?, error), ended_at=?,
		        executor=?, device_id=?, source_trust=?
		 WHERE id=?`,
		string(status), rawOrNil(result), nullIfEmpty(resultBlob),
		nullIfEmpty(errMsg), NowMillis(),
		string(e.Executor), nullIfEmpty(e.DeviceID), string(e.SourceTrust), id)
	if err != nil {
		return fmt.Errorf("store: 更新工具事件失败: %w", err)
	}
	return ensureAffected(res, "工具事件 "+id)
}

// ListByRun 按 Run 列出工具事件（UI 回放）。
func (r *ToolEventRepo) ListByRun(ctx context.Context, runID string) ([]model.ToolEvent, error) {
	return r.listWhere(ctx, "run_id=?", runID)
}

// FailOpenByRun 把一个 Run 下**未收口**的工具事件统一收口为 error 终态。
//
// 为什么必须有：引擎的 execTool 收尾（FinishToolEvent）与 Run 的终态迁移原本共用
// 被取消的 ctx，取消后 SQL 直接不执行且错误被 `_ =` 吞掉 → tool_events 永久停在
// pending/running。前端刷新回放（GET .../tool-events）照原样渲染成「执行中…」，
// 而 Run 早已结束 —— 这就是「界面停了但命令还在进行中」的另一半根因。
//
// 幂等：仅更新未终态行，重复调用影响 0 行不报错。
func (r *ToolEventRepo) FailOpenByRun(ctx context.Context, runID, reason string) (int, error) {
	if strings.TrimSpace(runID) == "" {
		return 0, errors.New("store: run_id 不能为空")
	}
	if reason == "" {
		reason = "Run 已结束，工具调用未产出结果"
	}
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE tool_events SET status=?, error=COALESCE(error, ?), ended_at=COALESCE(ended_at, ?)
		 WHERE run_id=? AND status IN ('pending','approved','running')`,
		string(model.ToolError), reason, NowMillis(), runID)
	if err != nil {
		return 0, fmt.Errorf("store: 收口未完成工具事件失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: 读取影响行数失败: %w", err)
	}
	return int(n), nil
}

// listWhere 按 where 条件查询工具事件（where 仅接受内部常量，参数一律占位符）。
func (r *ToolEventRepo) listWhere(ctx context.Context, where string, args ...any) ([]model.ToolEvent, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT id, run_id, message_id, tool_name, namespace, args_json, result_json, result_blob,
		        status, risk, executor, device_id, source_trust, approved_by, error, started_at, ended_at
		 FROM tool_events WHERE `+where+` ORDER BY started_at ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询工具事件失败: %w", err)
	}
	defer rows.Close()
	var out []model.ToolEvent
	for rows.Next() {
		var (
			ev       model.ToolEvent
			msgID    sql.NullString
			args     sql.NullString
			result   sql.NullString
			blob     sql.NullString
			device   sql.NullString
			approved sql.NullString
			errMsg   sql.NullString
			started  sql.NullInt64
			ended    sql.NullInt64
		)
		if err := rows.Scan(&ev.ID, &ev.RunID, &msgID, &ev.ToolName, &ev.Namespace,
			&args, &result, &blob, &ev.Status, &ev.Risk, &ev.Executor, &device,
			&ev.SourceTrust, &approved, &errMsg, &started, &ended); err != nil {
			return nil, fmt.Errorf("store: 解析工具事件失败: %w", err)
		}
		if msgID.Valid {
			ev.MessageID = msgID.String
		}
		if args.Valid {
			ev.ArgsJSON = json.RawMessage(args.String)
		}
		if result.Valid {
			ev.ResultJSON = json.RawMessage(result.String)
		}
		if blob.Valid {
			ev.ResultBlob = blob.String
		}
		if device.Valid {
			ev.DeviceID = device.String
		}
		if approved.Valid {
			ev.ApprovedBy = approved.String
		}
		if errMsg.Valid {
			ev.Error = errMsg.String
		}
		if started.Valid {
			v := started.Int64
			ev.StartedAt = &v
		}
		if ended.Valid {
			v := ended.Int64
			ev.EndedAt = &v
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// GetByRun 取某 Run 下的单个工具事件（审批决策校验用）。
//
// 不存在时返回 ErrNotFound（由 service 层翻译为 approval.not_found）。
func (r *ToolEventRepo) GetByRun(ctx context.Context, runID, id string) (*model.ToolEvent, error) {
	list, err := r.listWhere(ctx, `run_id=? AND id=?`, runID, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return &list[0], nil
}

// ============ 审批规则 ============

// ApprovalRuleRepo 会话库审批规则仓储（「本会话允许」）。
type ApprovalRuleRepo struct{ s *SessionDB }

// ApprovalRules 返回审批规则仓储。
func (s *SessionDB) ApprovalRules() *ApprovalRuleRepo { return &ApprovalRuleRepo{s: s} }

// List 列出本会话全部规则。
func (r *ApprovalRuleRepo) List(ctx context.Context) ([]model.ApprovalRule, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT id, tool_name, pattern, created_at FROM approval_rules ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询审批规则失败: %w", err)
	}
	defer rows.Close()
	var out []model.ApprovalRule
	for rows.Next() {
		var rule model.ApprovalRule
		if err := rows.Scan(&rule.ID, &rule.ToolName, &rule.Pattern, &rule.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: 解析审批规则失败: %w", err)
		}
		out = append(out, rule)
	}
	return out, rows.Err()
}

// Add 新增规则（同工具+同 pattern 幂等，由唯一索引保证）。
func (r *ApprovalRuleRepo) Add(ctx context.Context, toolName, pattern string) error {
	_, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO approval_rules(id, tool_name, pattern, created_at) VALUES(?,?,?,?)
		 ON CONFLICT(tool_name, pattern) DO NOTHING`,
		newUUID(), toolName, pattern, NowMillis())
	if err != nil {
		return fmt.Errorf("store: 新增审批规则失败: %w", err)
	}
	return nil
}

// Delete 删除规则。
func (r *ApprovalRuleRepo) Delete(ctx context.Context, id string) error {
	res, err := r.s.conn().ExecContext(ctx, `DELETE FROM approval_rules WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("store: 删除审批规则失败: %w", err)
	}
	return ensureAffected(res, "审批规则 "+id)
}

// ============ 审批（决策审计） ============

// ApprovalRepo 会话库审批仓储（记录人工决策，供审计与 UI 回放）。
type ApprovalRepo struct{ s *SessionDB }

// Approvals 返回审批仓储。
func (s *SessionDB) Approvals() *ApprovalRepo { return &ApprovalRepo{s: s} }

// Decide 记录一次审批决策（id 复用审批 ID = 工具事件 ID，幂等覆盖）。
func (r *ApprovalRepo) Decide(ctx context.Context, id, runID, toolEventID, decision, reason string) error {
	return r.DecideWithDevices(ctx, ApprovalDecision{
		ID: id, RunID: runID, ToolEventID: toolEventID,
		Decision: decision, Reason: reason,
	})
}

// ApprovalDecision 一次审批决策的完整审计记录（阶段 C-7 跨端仲裁）。
//
// RequestedByDevice / DecidedByDevice 为空串时写 NULL（表示服务端本地执行面），
// 因此既有的两参数式调用（Decide）行为完全不变。
type ApprovalDecision struct {
	ID          string
	RunID       string
	ToolEventID string
	Decision    string
	Reason      string
	// RequestedByDevice 发起该审批的执行端设备（空 = 服务端本地）。
	RequestedByDevice string
	// DecidedByDevice 实际做出决策的设备（空 = 浏览器/本地管理端）。
	DecidedByDevice string
}

// DecideWithDevices 记录决策并落库跨端审计列（requested_by_device / decided_by_device）。
func (r *ApprovalRepo) DecideWithDevices(ctx context.Context, d ApprovalDecision) error {
	if d.ID == "" || d.RunID == "" {
		return errors.New("store: 审批记录缺少必要字段")
	}
	now := NowMillis()
	_, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO approvals(id, run_id, tool_event_id, request_json, decision, reason,
		                       requested_at, decided_at, requested_by_device, decided_by_device)
		 VALUES(?,?,?,'{}',?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET decision=excluded.decision,
		   reason=excluded.reason, decided_at=excluded.decided_at,
		   requested_by_device=COALESCE(excluded.requested_by_device, requested_by_device),
		   decided_by_device=excluded.decided_by_device`,
		d.ID, d.RunID, d.ToolEventID, d.Decision, nullIfEmpty(d.Reason), now, now,
		nullIfEmpty(d.RequestedByDevice), nullIfEmpty(d.DecidedByDevice))
	if err != nil {
		return fmt.Errorf("store: 记录审批决策失败: %w", err)
	}
	return nil
}

// ApprovalRecord 审批决策审计记录（阶段 C-7：谁发起 / 谁决策）。
type ApprovalRecord struct {
	ID                string `json:"id"`
	RunID             string `json:"run_id"`
	ToolEventID       string `json:"tool_event_id"`
	Decision          string `json:"decision"`
	Reason            string `json:"reason"`
	RequestedAt       int64  `json:"requested_at"`
	DecidedAt         int64  `json:"decided_at"`
	RequestedByDevice string `json:"requested_by_device"` // 空 = 服务端本地执行面
	DecidedByDevice   string `json:"decided_by_device"`   // 空 = 浏览器/本地管理端
}

// Get 按 ID 读取审批审计记录（不存在返回 ErrNotFound；供 UI 回放与测试断言）。
func (r *ApprovalRepo) Get(ctx context.Context, id string) (*ApprovalRecord, error) {
	var (
		rec      ApprovalRecord
		decision sql.NullString
		reason   sql.NullString
		decided  sql.NullInt64
		reqDev   sql.NullString
		decDev   sql.NullString
	)
	err := r.s.conn().QueryRowContext(ctx,
		`SELECT id, run_id, tool_event_id, decision, reason, requested_at, decided_at,
		        requested_by_device, decided_by_device
		   FROM approvals WHERE id=?`, id).
		Scan(&rec.ID, &rec.RunID, &rec.ToolEventID, &decision, &reason, &rec.RequestedAt,
			&decided, &reqDev, &decDev)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查询审批记录失败: %w", err)
	}
	rec.Decision = decision.String
	rec.Reason = reason.String
	rec.DecidedAt = decided.Int64
	rec.RequestedByDevice = reqDev.String
	rec.DecidedByDevice = decDev.String
	return &rec, nil
}

// nullIfEmpty 空串写 NULL（便于 COALESCE 语义）。
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// rawOrNil 空 JSON 写 NULL。
func rawOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}
