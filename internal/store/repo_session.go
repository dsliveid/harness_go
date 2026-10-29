package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

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
	tool_call_id, attachments_json, token_count, usage_json, created_at`

func scanMessage(sc interface{ Scan(...any) error }) (model.Message, error) {
	var (
		m        model.Message
		runID    sql.NullString
		reason   sql.NullString
		toolCall sql.NullString
		toolID   sql.NullString
		attach   sql.NullString
		usage    sql.NullString
	)
	err := sc.Scan(&m.ID, &m.Seq, &runID, &m.Role, &m.Content, &reason,
		&toolCall, &toolID, &attach, &m.TokenCount, &usage, &m.CreatedAt)
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
	return ensureAffected(res, "Run "+id)
}

// ListUnfinished 列出未完成 Run（启动恢复用）。
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

// Finish 结束工具调用（写结果与终态）。
func (r *ToolEventRepo) Finish(ctx context.Context, id string, status model.ToolEventStatus,
	result json.RawMessage, resultBlob, errMsg string) error {
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE tool_events SET status=?, result_json=?, result_blob=?,
		        error=COALESCE(?, error), ended_at=?
		 WHERE id=?`,
		string(status), rawOrNil(result), nullIfEmpty(resultBlob),
		nullIfEmpty(errMsg), NowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: 更新工具事件失败: %w", err)
	}
	return ensureAffected(res, "工具事件 "+id)
}

// ListByRun 按 Run 列出工具事件（UI 回放）。
func (r *ToolEventRepo) ListByRun(ctx context.Context, runID string) ([]model.ToolEvent, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT id, run_id, message_id, tool_name, namespace, args_json, result_json, result_blob,
		        status, risk, executor, device_id, source_trust, approved_by, error, started_at, ended_at
		 FROM tool_events WHERE run_id=? ORDER BY started_at ASC`, runID)
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
