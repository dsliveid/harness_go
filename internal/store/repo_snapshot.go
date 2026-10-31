package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"harness-go/internal/model"
)

// ============ 全局快照仓储（主库，阶段 G / S10） ============
//
// 内容本体在 data/snapshots/blobs/<sha256>（全局 CAS，跨会话去重）；
// 本仓储只管快照行与 blob 引用计数。写入顺序与 blob 同铁律：先文件后库。

// ErrSnapshotNotFound 快照行不存在。
var ErrSnapshotNotFound = errors.New("store: 快照不存在")

// SnapshotsRepo 全局快照仓储。
type SnapshotsRepo struct{ s *MainStore }

// Snapshots 返回快照仓储。
func (s *MainStore) Snapshots() *SnapshotsRepo { return &SnapshotsRepo{s: s} }

// Insert 写入快照行并对涉及 blob 引用计数 +1（单事务）。
func (r *SnapshotsRepo) Insert(ctx context.Context, snap *model.Snapshot) error {
	if snap.ID == "" {
		return errors.New("store: 快照 ID 不能为空")
	}
	paths, err := json.Marshal(snap.Paths)
	if err != nil {
		return fmt.Errorf("store: 序列化快照路径失败: %w", err)
	}
	if snap.CreatedAt == 0 {
		snap.CreatedAt = NowMillis()
	}
	return r.s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO snapshots(id, session_id, run_id, tool_event_id, task_id,
			                       kind, paths_json, size, created_at)
			 VALUES(?,?,?,?,?,?,?,?,?)`,
			snap.ID, snap.SessionID, snap.RunID, snap.ToolEventID, snap.TaskID,
			snap.Kind, string(paths), snap.Size, snap.CreatedAt)
		if err != nil {
			return fmt.Errorf("store: 写入快照行失败: %w", err)
		}
		return bumpBlobRefs(ctx, tx, snap.Paths, +1)
	})
}

// Get 读取快照行。
func (r *SnapshotsRepo) Get(ctx context.Context, id string) (model.Snapshot, error) {
	row := r.s.conn().QueryRowContext(ctx,
		`SELECT id, session_id, run_id, tool_event_id, task_id, kind, paths_json, size, created_at
		 FROM snapshots WHERE id = ?`, id)
	return scanSnapshot(row)
}

// ListBySession 按会话列出快照（时间倒序）。
func (r *SnapshotsRepo) ListBySession(ctx context.Context, sessionID string, limit int) ([]model.Snapshot, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT id, session_id, run_id, tool_event_id, task_id, kind, paths_json, size, created_at
		 FROM snapshots WHERE session_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`,
		sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 列出会话快照失败: %w", err)
	}
	defer rows.Close()
	out := make([]model.Snapshot, 0, limit)
	for rows.Next() {
		snap, sErr := scanSnapshot(rows)
		if sErr != nil {
			return nil, sErr
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

// ListRecent 全局最近快照（toolapi.SnapshotStore.List 用；跨会话）。
func (r *SnapshotsRepo) ListRecent(ctx context.Context, limit int) ([]model.Snapshot, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT id, session_id, run_id, tool_event_id, task_id, kind, paths_json, size, created_at
		 FROM snapshots ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 列出最近快照失败: %w", err)
	}
	defer rows.Close()
	out := make([]model.Snapshot, 0, limit)
	for rows.Next() {
		snap, sErr := scanSnapshot(rows)
		if sErr != nil {
			return nil, sErr
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

// Delete 删除单条快照行并归还 blob 引用计数，返回引用归零的 blob（供 GC）。
func (r *SnapshotsRepo) Delete(ctx context.Context, id string) (freed []string, err error) {
	snap, err := r.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	err = r.s.withTx(ctx, func(tx *sql.Tx) error {
		res, dErr := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE id = ?`, id)
		if dErr != nil {
			return fmt.Errorf("store: 删除快照行失败: %w", dErr)
		}
		return ensureAffected(res, "快照 "+id)
	})
	if err != nil {
		return nil, err
	}
	return r.releaseBlobs(ctx, snap.Paths)
}

// DeleteBySession 删除会话的全部快照行（会话删除时调用），
// 返回引用归零的 blob（供 GC 物理清理）。
func (r *SnapshotsRepo) DeleteBySession(ctx context.Context, sessionID string) (freed []string, err error) {
	snaps, err := r.ListBySession(ctx, sessionID, 500)
	if err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return nil, nil
	}
	err = r.s.withTx(ctx, func(tx *sql.Tx) error {
		_, dErr := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE session_id = ?`, sessionID)
		if dErr != nil {
			return fmt.Errorf("store: 清理会话快照失败: %w", dErr)
		}
		for _, snap := range snaps {
			if bErr := bumpBlobRefs(ctx, tx, snap.Paths, -1); bErr != nil {
				return bErr
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, snap := range snaps {
		for _, b := range snap.Paths {
			if b.Hash == "" {
				continue
			}
			if _, ok := seen[b.Hash]; !ok {
				seen[b.Hash] = struct{}{}
				freed = append(freed, b.Hash)
			}
		}
	}
	return freed, nil
}

// ZeroRefBlobs 列出引用计数 ≤0 的 blob hash（GC 候选）。
func (r *SnapshotsRepo) ZeroRefBlobs(ctx context.Context) ([]string, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT hash FROM snapshot_blobs WHERE ref_count <= 0`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询零引用快照 blob 失败: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("store: 解析 blob hash 失败: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ForgetZeroRefs 删除引用计数 ≤0 的计数行（GC 已物理清理文件后调用）。
func (r *SnapshotsRepo) ForgetZeroRefs(ctx context.Context) (int64, error) {
	res, err := r.s.conn().ExecContext(ctx,
		`DELETE FROM snapshot_blobs WHERE ref_count <= 0`)
	if err != nil {
		return 0, fmt.Errorf("store: 清理零引用计数行失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// releaseBlobs 对一批路径绑定做 -1 计数，返回本次归零的 hash。
func (r *SnapshotsRepo) releaseBlobs(ctx context.Context, paths []model.SnapshotBinding) ([]string, error) {
	var freed []string
	err := r.s.withTx(ctx, func(tx *sql.Tx) error {
		if err := bumpBlobRefs(ctx, tx, paths, -1); err != nil {
			return err
		}
		for _, b := range paths {
			if b.Hash == "" {
				continue
			}
			var n int
			if qErr := tx.QueryRowContext(ctx,
				`SELECT ref_count FROM snapshot_blobs WHERE hash = ?`, b.Hash).Scan(&n); qErr != nil {
				continue // 计数行不存在 = 已被并发 GC，无碍
			}
			if n <= 0 {
				freed = append(freed, b.Hash)
			}
		}
		return nil
	})
	return freed, err
}

// bumpBlobRefs 事务内对路径绑定的 blob 引用计数 ±1（新 hash 首次 +1 时插行）。
func bumpBlobRefs(ctx context.Context, tx *sql.Tx, paths []model.SnapshotBinding, delta int) error {
	for _, b := range paths {
		if b.Hash == "" {
			continue
		}
		if delta > 0 {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO snapshot_blobs(hash, size, ref_count) VALUES(?,?,1)
				 ON CONFLICT(hash) DO UPDATE SET ref_count = ref_count + 1`,
				b.Hash, b.Size)
			if err != nil {
				return fmt.Errorf("store: 快照 blob 计数 +1 失败: %w", err)
			}
			continue
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE snapshot_blobs SET ref_count = ref_count - 1 WHERE hash = ?`, b.Hash)
		if err != nil {
			return fmt.Errorf("store: 快照 blob 计数 -1 失败: %w", err)
		}
	}
	return nil
}

type rowScanner interface{ Scan(...any) error }

func scanSnapshot(sc rowScanner) (model.Snapshot, error) {
	var (
		snap      model.Snapshot
		sessionID sql.NullString
		runID     sql.NullString
		eventID   sql.NullString
		taskID    sql.NullString
		pathsJSON string
	)
	if err := sc.Scan(&snap.ID, &sessionID, &runID, &eventID, &taskID,
		&snap.Kind, &pathsJSON, &snap.Size, &snap.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Snapshot{}, ErrSnapshotNotFound
		}
		return model.Snapshot{}, fmt.Errorf("store: 解析快照行失败: %w", err)
	}
	snap.SessionID = sessionID.String
	snap.RunID = runID.String
	snap.ToolEventID = eventID.String
	snap.TaskID = taskID.String
	if err := json.Unmarshal([]byte(pathsJSON), &snap.Paths); err != nil {
		return model.Snapshot{}, fmt.Errorf("store: 解析快照路径失败: %w", err)
	}
	return snap, nil
}
