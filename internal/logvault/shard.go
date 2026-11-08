package logvault

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，无 CGO（单二进制前提）
)

// schemaVersion 天库当前 schema 版本（存 PRAGMA user_version）。
//
// 演进策略：新版本只需在 ensureSchema 里追加幂等补列，然后提升本常量。
// 与业务库（//go:embed migrations 版本化）不同，天库是可丢弃数据，
// 历史库 schema 落后不影响业务正确性，故允许幂等补列而非严格版本迁移。
const schemaVersion = 1

// dayLayout 天键格式（同时也是天库文件名中的日期段）。
const dayLayout = "20060102"

// shardCacheSize 同时打开的天库上限（LRU）。
//
// 为什么是 3：一次查询最多跨 2 个天库（天边界），写入只碰当天，
// 保留 3 个足以覆盖「查昨天 + 写今天 + 查前天」的常见组合，且连接数极少。
const shardCacheSize = 3

// Shard 天分片库族的路由与连接管理。
//
// 与 store.SessionStore 的关键差异：天库**不做 integrity_check**。
// 理由：天库是可丢弃数据，完整性校验失败直接重建整天库即可；
// 而会话库承载业务事实，损坏必须立即暴露。
type Shard struct {
	root string // data/logs

	mu   sync.Mutex
	open map[string]*shardEntry
	seq  int64 // LRU 计数（单调递增，用于排序淘汰）
}

type shardEntry struct {
	db       *sql.DB
	lastUsed int64
}

// NewShard 创建天分片管理器（不建目录、不扫盘，纯惰性）。
func NewShard(root string) (*Shard, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("logvault: 日志根目录不能为空")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("logvault: 解析日志根目录失败: %w", err)
	}
	return &Shard{root: abs, open: make(map[string]*shardEntry, shardCacheSize)}, nil
}

// Root 返回日志库族根目录（bolb 目录、归档目录均在其下）。
func (s *Shard) Root() string { return s.root }

// BlobRoot 返回指定天键的正文 CAS 根目录（data/logs/blob/YYYYMMDD）。
func (s *Shard) BlobRoot(day string) string {
	return filepath.Join(s.root, "blob", day)
}

// ArchiveRoot 返回冷归档目录（data/logs/archive）。
func (s *Shard) ArchiveRoot() string { return filepath.Join(s.root, "archive") }

// DayOf 把时间戳（Unix 毫秒）转为天键 YYYYMMDD（本地时区）。
func DayOf(ms int64) string {
	return time.UnixMilli(ms).Format(dayLayout)
}

// DayOfTime 把时间转为天键。
func DayOfTime(t time.Time) string { return t.Format(dayLayout) }

// Today 返回今天的天键（本地时区）。
func Today() string { return time.Now().Format(dayLayout) }

// ParseDay 解析天键为当日零点（本地时区）。
func ParseDay(day string) (time.Time, error) {
	t, err := time.ParseInLocation(dayLayout, day, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("logvault: 天键 %q 非法: %w", day, err)
	}
	return t, nil
}

// DayStartMs 返回该天 00:00:00.000 的 Unix 毫秒。
func DayStartMs(day string) (int64, error) {
	t, err := ParseDay(day)
	if err != nil {
		return 0, err
	}
	return t.UnixMilli(), nil
}

// DayEndMs 返回该天 23:59:59.999 的 Unix 毫秒。
func DayEndMs(day string) (int64, error) {
	t, err := ParseDay(day)
	if err != nil {
		return 0, err
	}
	return t.Add(24*time.Hour - time.Millisecond).UnixMilli(), nil
}

// dbPath 天库文件路径。
func (s *Shard) dbPath(day string) string {
	return filepath.Join(s.root, "logs-"+day+".db")
}

// dsn 构造带统一 PRAGMA 的 DSN（对齐桌面端 logs.rs:305-311 的日志库专用参数）。
//
// 关键差异（相对业务库 store.dsn）：
//   - auto_vacuum=NONE：天库靠删整文件回收空间，不需要增量 VACUUM 维护；
//   - page_size=8192：日志行小且密，较大页减少 B-tree 深度。
func dsn(path string) string {
	return fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"+
			"&_pragma=synchronous(NORMAL)&_pragma=page_size(8192)"+
			"&_pragma=auto_vacuum(NONE)",
		filepath.ToSlash(path),
	)
}

// Day 取得（必要时创建）指定天库。day 必须形如 YYYYMMDD。
func (s *Shard) Day(ctx context.Context, day string) (*sql.DB, error) {
	if _, err := ParseDay(day); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if e, ok := s.open[day]; ok {
		s.seq++
		e.lastUsed = s.seq
		db := e.db
		s.mu.Unlock()
		return db, nil
	}
	s.mu.Unlock()

	db, err := s.openDay(ctx, day)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// 双检：并发打开同一天时复用先到者
	if e, ok := s.open[day]; ok {
		s.seq++
		e.lastUsed = s.seq
		_ = db.Close()
		return e.db, nil
	}
	s.evictLocked()
	s.seq++
	s.open[day] = &shardEntry{db: db, lastUsed: s.seq}
	return db, nil
}

// openDay 打开/创建单个天库并幂等升级 schema。
func (s *Shard) openDay(ctx context.Context, day string) (*sql.DB, error) {
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return nil, fmt.Errorf("logvault: 创建日志目录失败: %w", err)
	}
	path := s.dbPath(day)
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("logvault: 打开天库 %s 失败: %w", day, err)
	}
	// 天库单连接：日志写入是单协程批量提交，查询并发极低。
	// 多连接反而会放大 WAL 增长与锁竞争。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("logvault: 连接天库 %s 失败: %w", day, err)
	}
	if err := ensureSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("logvault: 天库 %s schema 初始化失败: %w", day, err)
	}
	return db, nil
}

// evictLocked 按 LRU 淘汰到 shardCacheSize 以内。调用方必须持锁。
func (s *Shard) evictLocked() {
	for len(s.open) >= shardCacheSize {
		var oldestDay string
		var oldestSeq int64
		for day, e := range s.open {
			if oldestDay == "" || e.lastUsed < oldestSeq {
				oldestDay, oldestSeq = day, e.lastUsed
			}
		}
		if oldestDay == "" {
			return
		}
		s.closeLocked(oldestDay)
	}
}

// closeLocked 关闭并移除天库连接。调用方必须持锁。
func (s *Shard) closeLocked(day string) {
	if e, ok := s.open[day]; ok {
		// 出池前收缩 WAL，避免 -wal/-shm 残留占盘
		_, _ = e.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
		_ = e.db.Close()
		delete(s.open, day)
	}
}

// ListDays 列出全部已存在的天键，**升序**（旧 → 新）。
//
// 实现即「读目录」：天库文件名就是时间索引，无需任何 DB 访问。
func (s *Shard) ListDays() []string {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "logs-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		day := strings.TrimSuffix(strings.TrimPrefix(name, "logs-"), ".db")
		if len(day) != 8 {
			continue
		}
		if _, err := ParseDay(day); err != nil {
			continue
		}
		out = append(out, day)
	}
	sort.Strings(out)
	return out
}

// AvailableRange 返回当前可查的最早/最晚天键（无天库时两者均为空串）。
//
// 用途：日志查询界面需要明示「日志在线保留 N 天（最早可查 YYYYMMDD）」，
// 让用户明确知道选择更早时间范围时是「已清理」而不是「没有数据」。
func (s *Shard) AvailableRange() (earliest, latest string) {
	days := s.ListDays()
	if len(days) == 0 {
		return "", ""
	}
	return days[0], days[len(days)-1]
}

// openDayReadOnly 只读打开天库**但不进入 LRU 缓存**。
//
// 为什么需要它：跨 30 天聚合要依次打开 30 个天库，而 LRU 容量只有 3 ——
// 走缓存会触发 30 次淘汰，每次淘汰都跑 wal_checkpoint(TRUNCATE)，
// 实测把 30 天聚合从预期 20ms 拖到 407ms。
//
// 只读场景用完即关，成本是一次 open+close（无 fsync、无 checkpoint），
// 远低于反复摘除缓存条目的代价。
func (s *Shard) openDayReadOnly(day string) (*sql.DB, error) {
	path := s.dbPath(day)
	if _, err := os.Stat(path); err != nil {
		return nil, err // 文件不存在：该天无数据或已清理
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// DropDay 物理删除指定天库（含其 -wal/-shm 与 blob 目录）。
//
// 用于保留期清理与「按 session 定向删除」后的天库整库回收。
// 若该天库正处于打开状态，先关闭再删（Windows 下打开中的文件无法删除）。
func (s *Shard) DropDay(day string) error {
	s.mu.Lock()
	s.closeLocked(day)
	s.mu.Unlock()

	base := s.dbPath(day)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(base + suffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("logvault: 删除天库 %s%s 失败: %w", day, suffix, err)
		}
	}
	if err := os.RemoveAll(s.BlobRoot(day)); err != nil {
		return fmt.Errorf("logvault: 删除 %s 正文目录失败: %w", day, err)
	}
	return nil
}

// Close 关闭全部已打开的天库连接（优雅关闭）。
func (s *Shard) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for day := range s.open {
		s.closeLocked(day)
	}
	return nil
}

// DaySizeBytes 统计单个天库的磁盘占用（db + wal + shm + blob 目录）。
func (s *Shard) DaySizeBytes(day string) int64 {
	var total int64
	base := s.dbPath(day)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(base + suffix); err == nil {
			total += fi.Size()
		}
	}
	blobRoot := s.BlobRoot(day)
	_ = filepath.WalkDir(blobRoot, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, sErr := d.Info(); sErr == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}

// TotalSizeBytes 统计整个日志库族占用（用于配额熔断判定）。
func (s *Shard) TotalSizeBytes() int64 {
	var total int64
	for _, day := range s.ListDays() {
		total += s.DaySizeBytes(day)
	}
	// 归档目录也计入配额（否则归档会把磁盘吃满而不触发熔断）
	_ = filepath.WalkDir(s.ArchiveRoot(), func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, sErr := d.Info(); sErr == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}
