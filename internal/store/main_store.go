package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
)

// MainStore 主库：跨会话共享数据。
//
// 表：sessions / users / api_tokens / providers / settings / schema_migrations
// + 阶段 C 设备域：devices / device_pair_codes / capabilities / session_leases
// 铁律：不对外暴露 *sql.DB，仓储方法在 repo_main.go / repo_provider.go / repo_device.go 中实现。
type MainStore struct {
	db   *sql.DB
	path string
}

// OpenMain 打开主库，执行迁移与完整性校验。
func OpenMain(ctx context.Context, dataDir string) (*MainStore, error) {
	p := filepath.Join(dataDir, "main.db")
	db, err := openDB(p, 4)
	if err != nil {
		return nil, err
	}
	if _, err := applyMigrations(ctx, db, "main"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: 主库迁移失败: %w", err)
	}
	if err := integrityCheck(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &MainStore{db: db, path: p}, nil
}

// Path 返回主库文件路径（日志/维护接口用）。
func (s *MainStore) Path() string { return s.path }

// Close 关闭主库连接（优雅关闭时先 walCheckpoint）。
func (s *MainStore) Close() error {
	if s.db == nil {
		return nil
	}
	walCheckpoint(s.db)
	return s.db.Close()
}

// conn 包内访问连接（仅供仓储实现与同包测试）。
func (s *MainStore) conn() *sql.DB { return s.db }

// withTx 在主库执行单库事务（禁止跨库，见 docs/数据存储方案.md §4）。
func (s *MainStore) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 主库事务开启失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 主库事务提交失败: %w", err)
	}
	return nil
}

// ErrNotFound 统一"未找到"哨兵错误（上层用 errors.Is 判断）。
var ErrNotFound = errors.New("store: 记录不存在")
