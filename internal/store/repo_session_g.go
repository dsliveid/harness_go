package store

// 阶段 G 会话域仓储：长任务（tasks / task_checkpoints）与回滚事件（rollbacks）。
//
// 长任务状态机的持久态在会话库（删会话即随目录清空，无孤儿）；
// 快照本体在主库 + 全局 CAS，检查点行只存 snapshot_id 引用。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"harness-go/internal/model"
)

// ErrTaskNotFound 长任务不存在。
var ErrTaskNotFound = errors.New("store: 长任务不存在")

// ErrRollbackNotFound 回滚事件不存在。
var ErrRollbackNotFound = errors.New("store: 回滚事件不存在")

// ============ 长任务 ============

// TasksRepo 长任务仓储。
type TasksRepo struct{ s *SessionDB }

// Tasks 返回长任务仓储。
func (s *SessionDB) Tasks() *TasksRepo { return &TasksRepo{s: s} }

// Upsert 写入或更新任务（状态机每次迁移整行覆盖，updated_at 由本层刷新）。
func (r *TasksRepo) Upsert(ctx context.Context, t *model.Task) error {
	if t.ID == "" {
		return errors.New("store: 任务 ID 不能为空")
	}
	if t.CreatedAt == 0 {
		t.CreatedAt = NowMillis()
	}
	t.UpdatedAt = NowMillis()
	steps, err := json.Marshal(t.Steps)
	if err != nil {
		return fmt.Errorf("store: 序列化任务步骤失败: %w", err)
	}
	_, err = r.s.conn().ExecContext(ctx,
		`INSERT INTO tasks(id, run_id, title, state, steps_json, meta_json, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET run_id=excluded.run_id, title=excluded.title,
		   state=excluded.state, steps_json=excluded.steps_json, meta_json=excluded.meta_json,
		   updated_at=excluded.updated_at`,
		t.ID, t.RunID, t.Title, string(t.State), string(steps),
		rawOrNil(t.MetaJSON), t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: 写入任务失败: %w", err)
	}
	return nil
}

// Get 读取任务。
func (r *TasksRepo) Get(ctx context.Context, id string) (model.Task, error) {
	row := r.s.conn().QueryRowContext(ctx,
		`SELECT id, run_id, title, state, steps_json, meta_json, created_at, updated_at
		 FROM tasks WHERE id = ?`, id)
	return scanTask(row)
}

// List 全部任务（时间正序）。
func (r *TasksRepo) List(ctx context.Context) ([]model.Task, error) {
	return r.listWhere(ctx, ``)
}

// ListRunningNonTerminal 在途任务（启动恢复收敛用：pending/running/paused）。
func (r *TasksRepo) ListRunningNonTerminal(ctx context.Context) ([]model.Task, error) {
	return r.listWhere(ctx, ` WHERE state IN ('pending','running','paused')`)
}

func (r *TasksRepo) listWhere(ctx context.Context, where string) ([]model.Task, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT id, run_id, title, state, steps_json, meta_json, created_at, updated_at
		 FROM tasks`+where+` ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: 列出任务失败: %w", err)
	}
	defer rows.Close()
	out := []model.Task{}
	for rows.Next() {
		t, tErr := scanTask(rows)
		if tErr != nil {
			return nil, tErr
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func scanTask(sc interface{ Scan(...any) error }) (model.Task, error) {
	var (
		t         model.Task
		runID     sql.NullString
		stepsJSON string
		metaJSON  sql.NullString
	)
	if err := sc.Scan(&t.ID, &runID, &t.Title, &t.State, &stepsJSON, &metaJSON,
		&t.CreatedAt, &t.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Task{}, ErrTaskNotFound
		}
		return model.Task{}, fmt.Errorf("store: 解析任务行失败: %w", err)
	}
	t.RunID = runID.String
	if err := json.Unmarshal([]byte(stepsJSON), &t.Steps); err != nil && stepsJSON != "[]" && stepsJSON != "" {
		return model.Task{}, fmt.Errorf("store: 解析任务步骤失败: %w", err)
	}
	if metaJSON.Valid && metaJSON.String != "" {
		t.MetaJSON = json.RawMessage(metaJSON.String)
	}
	return t, nil
}

// ============ 长任务检查点 ============

// TaskCheckpointsRepo 检查点仓储。
type TaskCheckpointsRepo struct{ s *SessionDB }

// TaskCheckpoints 返回检查点仓储。
func (s *SessionDB) TaskCheckpoints() *TaskCheckpointsRepo { return &TaskCheckpointsRepo{s: s} }

// Add 登记检查点。
func (r *TaskCheckpointsRepo) Add(ctx context.Context, cp *model.TaskCheckpoint) error {
	if cp.ID == "" || cp.TaskID == "" {
		return errors.New("store: 检查点 ID 与任务 ID 不能为空")
	}
	if cp.CreatedAt == 0 {
		cp.CreatedAt = NowMillis()
	}
	_, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO task_checkpoints(id, task_id, run_id, snapshot_id, note, created_at)
		 VALUES(?,?,?,?,?,?)`,
		cp.ID, cp.TaskID, cp.RunID, cp.SnapshotID, cp.Note, cp.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: 写入检查点失败: %w", err)
	}
	return nil
}

// ListByTask 按任务列出检查点（时间正序）。
func (r *TaskCheckpointsRepo) ListByTask(ctx context.Context, taskID string) ([]model.TaskCheckpoint, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT id, task_id, run_id, snapshot_id, note, created_at
		 FROM task_checkpoints WHERE task_id = ? ORDER BY created_at ASC, id ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: 列出检查点失败: %w", err)
	}
	defer rows.Close()
	out := []model.TaskCheckpoint{}
	for rows.Next() {
		var cp model.TaskCheckpoint
		var runID, snapID sql.NullString
		if err := rows.Scan(&cp.ID, &cp.TaskID, &runID, &snapID, &cp.Note, &cp.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: 解析检查点行失败: %w", err)
		}
		cp.RunID = runID.String
		cp.SnapshotID = snapID.String
		out = append(out, cp)
	}
	return out, rows.Err()
}

// ============ 回滚事件 ============

// RollbacksRepo 回滚事件仓储。
type RollbacksRepo struct{ s *SessionDB }

// Rollbacks 返回回滚事件仓储。
func (s *SessionDB) Rollbacks() *RollbacksRepo { return &RollbacksRepo{s: s} }

// Insert 登记回滚事件。
func (r *RollbacksRepo) Insert(ctx context.Context, rb *model.Rollback) error {
	if rb.ID == "" {
		return errors.New("store: 回滚事件 ID 不能为空")
	}
	if rb.CreatedAt == 0 {
		rb.CreatedAt = NowMillis()
	}
	affected, err := json.Marshal(rb.Affected)
	if err != nil {
		return fmt.Errorf("store: 序列化回滚快照对失败: %w", err)
	}
	restored, err := json.Marshal(rb.Restored)
	if err != nil {
		return fmt.Errorf("store: 序列化回滚写回结果失败: %w", err)
	}
	_, err = r.s.conn().ExecContext(ctx,
		`INSERT INTO rollbacks(id, mode, target, affected_json, hidden_from_seq,
		                        restored_json, created_at, reapplied_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		rb.ID, string(rb.Mode), rb.Target, string(affected), rb.HiddenFromSeq,
		string(restored), rb.CreatedAt, rb.ReappliedAt)
	if err != nil {
		return fmt.Errorf("store: 写入回滚事件失败: %w", err)
	}
	return nil
}

// Get 读取回滚事件。
func (r *RollbacksRepo) Get(ctx context.Context, id string) (model.Rollback, error) {
	row := r.s.conn().QueryRowContext(ctx,
		`SELECT id, mode, target, affected_json, hidden_from_seq, restored_json, created_at, reapplied_at
		 FROM rollbacks WHERE id = ?`, id)
	return scanRollback(row)
}

// List 全部回滚事件（时间倒序）。
func (r *RollbacksRepo) List(ctx context.Context) ([]model.Rollback, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT id, mode, target, affected_json, hidden_from_seq, restored_json, created_at, reapplied_at
		 FROM rollbacks ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: 列出回滚事件失败: %w", err)
	}
	defer rows.Close()
	out := []model.Rollback{}
	for rows.Next() {
		rb, rErr := scanRollback(rows)
		if rErr != nil {
			return nil, rErr
		}
		out = append(out, rb)
	}
	return out, rows.Err()
}

// MarkReapplied 标记回滚事件已被 reapply（幂等：已 reapply 返回错误防重复）。
func (r *RollbacksRepo) MarkReapplied(ctx context.Context, id string) error {
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE rollbacks SET reapplied_at = ? WHERE id = ? AND reapplied_at IS NULL`,
		NowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: 标记 reapply 失败: %w", err)
	}
	return ensureAffected(res, "回滚事件 "+id+"（可能已被 reapply）")
}

func scanRollback(sc interface{ Scan(...any) error }) (model.Rollback, error) {
	var (
		rb           model.Rollback
		affectedJSON string
		restoredJSON string
		reappliedAt  sql.NullInt64
	)
	if err := sc.Scan(&rb.ID, &rb.Mode, &rb.Target, &affectedJSON, &rb.HiddenFromSeq,
		&restoredJSON, &rb.CreatedAt, &reappliedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Rollback{}, ErrRollbackNotFound
		}
		return model.Rollback{}, fmt.Errorf("store: 解析回滚事件失败: %w", err)
	}
	if err := json.Unmarshal([]byte(affectedJSON), &rb.Affected); err != nil {
		return model.Rollback{}, fmt.Errorf("store: 解析回滚快照对失败: %w", err)
	}
	if err := json.Unmarshal([]byte(restoredJSON), &rb.Restored); err != nil {
		return model.Rollback{}, fmt.Errorf("store: 解析回滚写回结果失败: %w", err)
	}
	if reappliedAt.Valid {
		t := reappliedAt.Int64
		rb.ReappliedAt = &t
	}
	return rb, nil
}
