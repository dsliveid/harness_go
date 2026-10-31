// Package store 实现数据持久化：主库（跨会话）+ 每会话一库 + 会话级 CAS。
//
// 设计要点（docs/数据存储方案.md v2.0）：
//   - 主库 data/main.db：sessions / users / api_tokens / providers / settings；
//     设备域（阶段 C）：devices / device_pair_codes / capabilities / session_leases
//   - 会话库 data/sessions/<sid>/session.db：messages / runs / tool_events / approvals / ...
//   - 会话级 blob：data/sessions/<sid>/blobs/<xx>/<sha256>（无全局 ref_count）
//   - 零跨库事务：所有事务都在单库内完成；跨库一致性靠"最终一致"（摘要回写）
//
// 铁律：本包不向上暴露 *sql.DB，只暴露仓储方法与显式事务函数。
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，无 CGO（单二进制交叉编译前提）
)

const driverName = "sqlite"

// NowMillis 统一时间戳单位（Unix 毫秒）。
func NowMillis() int64 { return time.Now().UnixMilli() }

// dsn 构造带统一 PRAGMA 的 SQLite DSN。
//
// auto_vacuum 必须在建表前生效，因此放在 DSN 而非建表后 PRAGMA。
func dsn(path string) string {
	return fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"+
			"&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"+
			"&_pragma=auto_vacuum(INCREMENTAL)",
		filepath.ToSlash(path),
	)
}

// openDB 打开 SQLite 连接池并验证可连接。
func openDB(path string, maxOpenConns int) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("store: 创建目录 %s 失败: %w", filepath.Dir(path), err)
	}
	db, err := sql.Open(driverName, dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: 打开 %s 失败: %w", path, err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxOpenConns)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: 连接 %s 失败: %w", path, err)
	}
	return db, nil
}

// integrityCheck 启动校验（docs/数据存储方案.md §5）。
func integrityCheck(db *sql.DB) error {
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("store: integrity_check 执行失败: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("store: 数据库完整性校验未通过: %s", result)
	}
	return nil
}

// walCheckpoint 收缩 WAL，避免大量会话库残留 -wal/-shm 文件。
func walCheckpoint(db *sql.DB) {
	_, _ = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
}
