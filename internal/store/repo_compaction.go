package store

// 上下文压缩仓储（session_compactions）。
//
// # 职责边界
//
// 本文件只负责**存取**，不做任何策略判断：
//   - 什么时候该压缩、压缩哪一段、摘要写什么 —— 全部由 compact 包决定；
//   - 这里只保证「写入顺序合法」与「读取语义确定」。
//
// 为什么要这样切：压缩策略会随模型上下文窗口、用户偏好、成本模型不断调整，
// 而存储语义应当稳定。混在一起会导致「改策略要动 SQL」，是最常见的腐化起点。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"harness-go/internal/model"
)

// ErrCompactionOutOfOrder 压缩区间与既有记录重叠或倒序。
//
// 这是**结构性错误**而非业务错误：一旦允许乱序，回放历史时摘要与消息的
// 相对顺序就会错乱，模型看到的时间线与真实发生顺序不一致。
// 因此宁可直接失败，也不静默接受。
var ErrCompactionOutOfOrder = errors.New("store: 压缩区间必须严格递增")

// CompactionRepo 压缩记录仓储。
type CompactionRepo struct{ s *SessionDB }

// Compactions 返回压缩仓储。
func (s *SessionDB) Compactions() *CompactionRepo { return &CompactionRepo{s: s} }

// Create 写入一条压缩记录。
//
// 校验 EndSeq 严格大于已存在的最大 EndSeq：区间必须单调递增且不重叠。
// 校验放在事务内，避免并发压缩产生重叠区间（两个 Run 同时触发压缩时）。
func (r *CompactionRepo) Create(ctx context.Context, c *model.Compaction) error {
	if c == nil {
		return errors.New("store: 压缩记录不能为空")
	}
	if c.ID == "" {
		return errors.New("store: 压缩记录 ID 不能为空")
	}
	if c.EndSeq <= 0 || c.StartSeq <= 0 || c.EndSeq < c.StartSeq {
		return fmt.Errorf("store: 压缩区间非法 [%d,%d]", c.StartSeq, c.EndSeq)
	}
	if c.CreatedAt == 0 {
		c.CreatedAt = NowMillis()
	}
	return r.s.withTx(ctx, func(tx *sql.Tx) error {
		var maxEnd sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT MAX(end_seq) FROM session_compactions`).Scan(&maxEnd); err != nil {
			return fmt.Errorf("store: 读取最大 end_seq 失败: %w", err)
		}
		if maxEnd.Valid && c.EndSeq <= maxEnd.Int64 {
			return fmt.Errorf("%w: 新区间 end_seq=%d 不大于已有最大 %d",
				ErrCompactionOutOfOrder, c.EndSeq, maxEnd.Int64)
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO session_compactions(id, start_seq, end_seq, summary_markdown,
			                                replaced_messages, replaced_tokens, model_ref, created_at)
			 VALUES(?,?,?,?,?,?,?,?)`,
			c.ID, c.StartSeq, c.EndSeq, c.SummaryMarkdown,
			c.ReplacedMessages, c.ReplacedTokens, c.ModelRef, c.CreatedAt)
		if err != nil {
			return fmt.Errorf("store: 写入压缩记录失败: %w", err)
		}
		return nil
	})
}

// List 按 end_seq 升序返回全部压缩记录。
//
// 升序是回放的**必需**顺序：历史重建要按时间线依次把摘要插入。
func (r *CompactionRepo) List(ctx context.Context) ([]model.Compaction, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT id, start_seq, end_seq, summary_markdown,
		        replaced_messages, replaced_tokens, model_ref, created_at
		 FROM session_compactions ORDER BY end_seq ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询压缩记录失败: %w", err)
	}
	defer rows.Close()

	var out []model.Compaction
	for rows.Next() {
		var c model.Compaction
		if err := rows.Scan(&c.ID, &c.StartSeq, &c.EndSeq, &c.SummaryMarkdown,
			&c.ReplacedMessages, &c.ReplacedTokens, &c.ModelRef, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: 扫描压缩记录失败: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LatestEndSeq 返回已压缩的最大 seq（0 = 从未压缩过）。
//
// 这是压缩引擎唯一需要的高频查询，单列走索引，避免每次加载全部记录。
func (r *CompactionRepo) LatestEndSeq(ctx context.Context) (int64, error) {
	var maxEnd sql.NullInt64
	err := r.s.conn().QueryRowContext(ctx,
		`SELECT MAX(end_seq) FROM session_compactions`).Scan(&maxEnd)
	if err != nil {
		return 0, fmt.Errorf("store: 读取最大 end_seq 失败: %w", err)
	}
	if !maxEnd.Valid {
		return 0, nil
	}
	return maxEnd.Int64, nil
}
