package store

import (
	"context"
	"fmt"
	"os"
)

// DBStats 单个 SQLite 库的统计信息（维护接口用）。
type DBStats struct {
	Path          string `json:"path"`
	PageSize      int64  `json:"page_size"`
	PageCount     int64  `json:"page_count"`
	FreelistCount int64  `json:"freelist_count"`
	FileSizeBytes int64  `json:"file_size_bytes"`
}

// Stats 返回主库统计。
func (s *MainStore) Stats(ctx context.Context) (DBStats, error) {
	return s.stats(ctx)
}

func (s *MainStore) stats(ctx context.Context) (DBStats, error) {
	st := DBStats{Path: s.path}
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&st.PageSize); err != nil {
		return st, fmt.Errorf("store: 读取 page_size 失败: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&st.PageCount); err != nil {
		return st, fmt.Errorf("store: 读取 page_count 失败: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&st.FreelistCount); err != nil {
		return st, fmt.Errorf("store: 读取 freelist_count 失败: %w", err)
	}
	if fi, err := os.Stat(s.path); err == nil {
		st.FileSizeBytes = fi.Size()
	}
	return st, nil
}

// Vacuum 执行 VACUUM（需独占，禁止在 Run 执行期间调用）。
func (s *MainStore) Vacuum(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("store: VACUUM 失败: %w", err)
	}
	walCheckpoint(s.db)
	return nil
}
