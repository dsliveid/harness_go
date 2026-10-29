package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SessionStore 管理会话库：路由 + LRU 连接池 + 惰性建库 + 惰性迁移。
//
// 关键约束（docs/数据存储方案.md §2.3）：
//   - 空会话不建库文件，首次真正读写时才创建
//   - 同时打开的会话库不超过 MaxOpen，超限淘汰最久未用者（出池前 checkpoint）
//   - 启动时**不**扫描全部会话库；迁移在打开时惰性执行
type SessionStore struct {
	dataDir   string
	maxOpen   int
	idleEvict time.Duration

	mu   sync.Mutex
	open map[string]*sessionEntry
}

type sessionEntry struct {
	db       *sql.DB
	dir      string
	lastUsed time.Time
}

// SessionStoreOptions 会话库管理器配置。
type SessionStoreOptions struct {
	DataDir   string
	MaxOpen   int           // 默认 8
	IdleEvict time.Duration // 默认 10min
}

// OpenSessionStore 创建会话库管理器（不做全量扫描）。
func OpenSessionStore(opts SessionStoreOptions) (*SessionStore, error) {
	if opts.DataDir == "" {
		return nil, fmt.Errorf("store: DataDir 不能为空")
	}
	if opts.MaxOpen <= 0 {
		opts.MaxOpen = 8
	}
	if opts.IdleEvict <= 0 {
		opts.IdleEvict = 10 * time.Minute
	}
	if err := os.MkdirAll(filepath.Join(opts.DataDir, "sessions"), 0o755); err != nil {
		return nil, fmt.Errorf("store: 创建 sessions 目录失败: %w", err)
	}
	return &SessionStore{
		dataDir:   opts.DataDir,
		maxOpen:   opts.MaxOpen,
		idleEvict: opts.IdleEvict,
		open:      make(map[string]*sessionEntry, opts.MaxOpen),
	}, nil
}

// SessionDir 返回会话目录（sessions/<sid>）。
func (s *SessionStore) SessionDir(sessionID string) string {
	return filepath.Join(s.dataDir, "sessions", sessionID)
}

// BlobRoot 返回会话级 CAS 根目录（sessions/<sid>/blobs）。
func (s *SessionStore) BlobRoot(sessionID string) string {
	return filepath.Join(s.SessionDir(sessionID), "blobs")
}

// Acquire 取得会话库连接（不存在则惰性创建并迁移）。
// 调用方无需归还：连接由 LRU 池管理，超限/闲置时自动出池。
func (s *SessionStore) Acquire(ctx context.Context, sessionID string) (*SessionDB, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("store: sessionID 不能为空")
	}
	s.mu.Lock()
	if e, ok := s.open[sessionID]; ok {
		e.lastUsed = time.Now()
		db, dir := e.db, e.dir
		s.mu.Unlock()
		return &SessionDB{db: db, SessionID: sessionID, Dir: dir}, nil
	}
	s.mu.Unlock()

	// 未命中：打开（必要时建库 + 迁移）。此处不做全局锁，允许并发打开不同会话。
	db, dir, err := s.openSessionDB(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// 双检：并发打开同一会话时，后到者复用先到的连接
	if e, ok := s.open[sessionID]; ok {
		e.lastUsed = time.Now()
		_ = db.Close()
		return &SessionDB{db: e.db, SessionID: sessionID, Dir: e.dir}, nil
	}
	// 淘汰：先清闲置超时，再按 LRU 数量上限淘汰
	s.evictLocked()
	s.open[sessionID] = &sessionEntry{db: db, dir: dir, lastUsed: time.Now()}
	return &SessionDB{db: db, SessionID: sessionID, Dir: dir}, nil
}

// evictLocked 调用方必须持锁。
func (s *SessionStore) evictLocked() {
	now := time.Now()
	for id, e := range s.open {
		if now.Sub(e.lastUsed) > s.idleEvict {
			s.closeEntry(id, e)
		}
	}
	for len(s.open) >= s.maxOpen {
		var oldestID string
		var oldest time.Time
		for id, e := range s.open {
			if oldestID == "" || e.lastUsed.Before(oldest) {
				oldestID, oldest = id, e.lastUsed
			}
		}
		if oldestID == "" {
			return
		}
		s.closeEntry(oldestID, s.open[oldestID])
	}
}

func (s *SessionStore) closeEntry(id string, e *sessionEntry) {
	walCheckpoint(e.db) // 出池前收缩 WAL，避免残留 -wal/-shm
	_ = e.db.Close()
	delete(s.open, id)
}

// openSessionDB 打开（必要时创建）单个会话库并迁移到最新 schema。
func (s *SessionStore) openSessionDB(ctx context.Context, sessionID string) (*sql.DB, string, error) {
	dir := s.SessionDir(sessionID)
	dbPath := filepath.Join(dir, "session.db")
	db, err := openDB(dbPath, 1) // 会话库单写连接：会话内串行
	if err != nil {
		return nil, "", err
	}
	if _, err := applyMigrations(ctx, db, "session"); err != nil {
		_ = db.Close()
		return nil, "", fmt.Errorf("store: 会话库 %s 迁移失败: %w", sessionID, err)
	}
	if err := integrityCheck(db); err != nil {
		_ = db.Close()
		return nil, "", err
	}
	return db, dir, nil
}

// OpenCount 当前已打开的会话库数量（维护接口/测试用）。
func (s *SessionStore) OpenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.open)
}

// Close 关闭全部会话库连接。
func (s *SessionStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for id, e := range s.open {
		walCheckpoint(e.db)
		if err := e.db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(s.open, id)
	}
	return firstErr
}

// DeleteSessionDir 删除整个会话目录（主库行删除后调用；孤儿由启动清理）。
func (s *SessionStore) DeleteSessionDir(sessionID string) error {
	s.mu.Lock()
	if e, ok := s.open[sessionID]; ok {
		s.closeEntry(sessionID, e)
	}
	s.mu.Unlock()
	return os.RemoveAll(s.SessionDir(sessionID))
}

// SessionDB 单个会话库的句柄。
type SessionDB struct {
	db        *sql.DB
	SessionID string
	Dir       string
}

// conn 包内访问连接（仅供仓储实现与同包测试）。
func (s *SessionDB) conn() *sql.DB { return s.db }

// withTx 在会话库执行单库事务。
func (s *SessionDB) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 会话库事务开启失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 会话库事务提交失败: %w", err)
	}
	return nil
}

// EnsureMeta 写入 session_meta（首建库时由上层在创建会话后调用）。
func (s *SessionDB) EnsureMeta(ctx context.Context, title string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO session_meta(id, session_id, title, created_at) VALUES(1,?,?,?)
		 ON CONFLICT(id) DO NOTHING`,
		s.SessionID, title, NowMillis())
	if err != nil {
		return fmt.Errorf("store: 写入 session_meta 失败: %w", err)
	}
	return nil
}
