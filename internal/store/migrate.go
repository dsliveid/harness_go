package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

// migrationsFS 内嵌全部迁移脚本（单一真源）。
// 目录：migrations/main/*.sql（主库版本线）、migrations/session/*.sql（会话库版本线）
//
//go:embed migrations
var migrationsFS embed.FS

// migration 一条迁移。
type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations 读取指定版本线下的迁移脚本，按 version 升序返回。
// 文件名约定：<4 位版本号>_<名称>.sql，例如 0001_init.sql。
func loadMigrations(sub string) ([]migration, error) {
	dir := path.Join("migrations", sub)
	entries, err := fs.ReadDir(migrationsFS, dir)
	if err != nil {
		return nil, fmt.Errorf("store: 读取迁移目录 %s 失败: %w", dir, err)
	}
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".sql")
		idx := strings.Index(base, "_")
		if idx <= 0 {
			return nil, fmt.Errorf("store: 迁移文件名非法（应为 NNNN_name.sql）: %s", e.Name())
		}
		ver, err := strconv.Atoi(base[:idx])
		if err != nil || ver <= 0 {
			return nil, fmt.Errorf("store: 迁移版本号非法: %s", e.Name())
		}
		body, err := migrationsFS.ReadFile(path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("store: 读取 %s 失败: %w", e.Name(), err)
		}
		out = append(out, migration{version: ver, name: base[idx+1:], sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("store: 迁移版本重复: %d", out[i].version)
		}
	}
	return out, nil
}

// ensureMigrationTable 保证版本表存在（先于任何迁移）。
func ensureMigrationTable(ctx context.Context, db *sql.DB) error {
	const ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at INTEGER NOT NULL
	)`
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("store: 创建 schema_migrations 失败: %w", err)
	}
	return nil
}

// applyMigrations 执行指定版本线上尚未应用的迁移，返回本次应用数量。
//
// 每个迁移在独立事务内执行：SQL 与版本记录原子生效，失败即回滚。
// 迁移 SQL 文件内不得包含 BEGIN/COMMIT（由本函数控制事务边界）。
func applyMigrations(ctx context.Context, db *sql.DB, sub string) (int, error) {
	if err := ensureMigrationTable(ctx, db); err != nil {
		return 0, err
	}
	all, err := loadMigrations(sub)
	if err != nil {
		return 0, err
	}

	applied := make(map[int]bool, len(all))
	rows, err := db.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return 0, fmt.Errorf("store: 查询已应用迁移失败: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return 0, err
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	count := 0
	for _, m := range all {
		if applied[m.version] {
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func applyOne(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 迁移 %d 开启事务失败: %w", m.version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("store: 迁移 %d(%s) 执行失败: %w", m.version, m.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations(version, name, applied_at) VALUES(?,?,?)",
		m.version, m.name, NowMillis(),
	); err != nil {
		return fmt.Errorf("store: 迁移 %d 记录版本失败: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 迁移 %d 提交失败: %w", m.version, err)
	}
	return nil
}

// SchemaVersion 返回当前已应用的最大迁移版本。
func SchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&v); err != nil {
		return 0, fmt.Errorf("store: 查询 schema 版本失败: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// RequiredSchemaVersion 返回二进制要求的最低 schema 版本（各版本线取最大）。
func RequiredSchemaVersion(sub string) (int, error) {
	all, err := loadMigrations(sub)
	if err != nil {
		return 0, err
	}
	if len(all) == 0 {
		return 0, nil
	}
	return all[len(all)-1].version, nil
}
