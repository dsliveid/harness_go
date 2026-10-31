package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenMainCreatesSchema(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	m, err := OpenMain(ctx, dir)
	if err != nil {
		t.Fatalf("OpenMain 失败: %v", err)
	}
	defer m.Close()

	// 主库普通表应有 16 张：
	//   0001 的 6 张（sessions/users/api_tokens/providers/settings/schema_migrations）
	// + 0002 的设备 4 张（devices/device_pair_codes/capabilities/session_leases）
	// + 0003 的检索 4 张（retrieval_docs/retrieval_vectors/index_meta + FTS5 虚表 retrieval_fts）
	// + 0005 的快照 2 张（snapshots/snapshot_blobs）
	// 注：FTS5 external content 模式的影子表（_data/_idx/_docsize/_config）同样以 table 类型登记，
	// 故这里用「至少 16」断言，具体影子表数量随 SQLite 版本可能变化。
	var n int
	if err := m.conn().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 16 {
		t.Fatalf("主库表数量期望至少 16，得到 %d", n)
	}

	v, err := SchemaVersion(ctx, m.conn())
	if err != nil {
		t.Fatal(err)
	}
	if v != 5 {
		t.Fatalf("主库 schema 版本期望 5，得到 %d", v)
	}
}

// 迁移必须幂等：重复打开不重复应用。
func TestMigrationIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	m1, err := OpenMain(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	var first int
	if err := m1.conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := m1.Close(); err != nil {
		t.Fatal(err)
	}

	m2, err := OpenMain(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	var second int
	if err := m2.conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&second); err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("迁移不幂等: 第一次 %d 行，第二次 %d 行", first, second)
	}
}

// 空会话不得建库：只有真正 Acquire 时才创建 session.db。
func TestSessionDBCreatedLazily(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	ss, err := OpenSessionStore(SessionStoreOptions{DataDir: dir, MaxOpen: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()

	const sid = "sess-lazy-1"
	dbPath := filepath.Join(ss.SessionDir(sid), "session.db")
	if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Acquire 前不应存在会话库文件: %v", err)
	}

	sdb, err := ss.Acquire(ctx, sid)
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("Acquire 后应创建会话库文件: %v", err)
	}

	// 会话库应有 12 张表（session_meta/messages/runs/tool_events/approvals/approval_rules/blobs/session_settings/schema_migrations
	// + 0004 的 tasks/task_checkpoints/rollbacks）
	var n int
	if err := sdb.conn().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 12 {
		t.Fatalf("会话库表数量期望 12，得到 %d", n)
	}

	// 多端预埋字段必须存在
	for _, col := range []string{"executor", "device_id", "parent_run_id"} {
		var cnt int
		if err := sdb.conn().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name=?`, col).Scan(&cnt); err != nil {
			t.Fatal(err)
		}
		if cnt != 1 {
			t.Fatalf("runs 缺少预埋字段 %s", col)
		}
	}
	for _, col := range []string{"executor", "device_id", "source_trust"} {
		var cnt int
		if err := sdb.conn().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info('tool_events') WHERE name=?`, col).Scan(&cnt); err != nil {
			t.Fatal(err)
		}
		if cnt != 1 {
			t.Fatalf("tool_events 缺少预埋字段 %s", col)
		}
	}
}

// LRU 池：打开 20 个会话，同时打开数不得超过上限。
func TestSessionPoolLimit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	const maxOpen = 8

	ss, err := OpenSessionStore(SessionStoreOptions{
		DataDir:   dir,
		MaxOpen:   maxOpen,
		IdleEvict: time.Hour, // 排除闲置淘汰干扰
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()

	for i := 0; i < 20; i++ {
		sid := "sess-" + strings.Repeat("a", i%3) + string(rune('0'+i%10)) + "-" + string(rune('A'+i))
		if _, err := ss.Acquire(ctx, sid); err != nil {
			t.Fatalf("Acquire(%s) 失败: %v", sid, err)
		}
		if got := ss.OpenCount(); got > maxOpen {
			t.Fatalf("同时打开会话库数超限: %d > %d", got, maxOpen)
		}
	}
	if got := ss.OpenCount(); got > maxOpen {
		t.Fatalf("最终打开数超限: %d > %d", got, maxOpen)
	}
}

func TestBlobDedupAndRead(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	root := filepath.Join(dir, "sessions", "s1", "blobs")
	tmp := filepath.Join(dir, "tmp")

	b, err := NewSessionBlobs(root, tmp, 64<<20)
	if err != nil {
		t.Fatal(err)
	}

	content := []byte("hello harness-go blob")
	ref1, err := b.Put(ctx, bytes.NewReader(content), "text/plain")
	if err != nil {
		t.Fatalf("第一次 Put 失败: %v", err)
	}
	ref2, err := b.Put(ctx, bytes.NewReader(content), "text/plain")
	if err != nil {
		t.Fatalf("第二次 Put 失败: %v", err)
	}
	if ref1.Hash != ref2.Hash {
		t.Fatalf("同内容应得到相同 hash: %s vs %s", ref1.Hash, ref2.Hash)
	}
	if ref1.Size != int64(len(content)) {
		t.Fatalf("size 错误: %d", ref1.Size)
	}

	// 磁盘上只有 1 份
	var files int
	_ = filepath.Walk(root, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			files++
		}
		return nil
	})
	if files != 1 {
		t.Fatalf("同内容应只落 1 份文件，实际 %d", files)
	}

	// tmp 无残留
	entries, _ := os.ReadDir(tmp)
	if len(entries) != 0 {
		t.Fatalf("tmp 应无残留 .part，实际 %d", len(entries))
	}

	rc, err := b.Open(ctx, ref1.Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	buf := make([]byte, len(content))
	if _, err := rc.Read(buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, content) {
		t.Fatalf("读取内容不一致: %q", buf)
	}

	meta, err := b.Stat(ctx, ref1.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Size != int64(len(content)) {
		t.Fatalf("Stat size 错误: %d", meta.Size)
	}
}

func TestBlobTooLargeRejected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	root := filepath.Join(dir, "blobs")
	tmp := filepath.Join(dir, "tmp")

	const limit = 16
	b, err := NewSessionBlobs(root, tmp, limit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Put(ctx, bytes.NewReader(make([]byte, limit+1)), "application/octet-stream")
	if !errors.Is(err, ErrBlobTooLarge) {
		t.Fatalf("期望 ErrBlobTooLarge，得到 %v", err)
	}
	// 超限不得留下任何文件
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Fatalf("超限后 tmp 应无残留，实际 %d", len(entries))
	}
	var files int
	_ = filepath.Walk(root, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			files++
		}
		return nil
	})
	if files != 0 {
		t.Fatalf("超限不应落盘，实际 %d 个文件", files)
	}
}

func TestCleanupTmpRemovesPartFiles(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "a.part"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := CleanupTmp(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应清理 1 个 .part，实际 %d", n)
	}
	if _, err := os.Stat(filepath.Join(tmp, "keep.txt")); err != nil {
		t.Fatal("非 .part 文件不应被清理")
	}
}

// 消息 seq 在会话库内唯一（分库后不再需要 session_id 复合唯一）。
func TestMessageSeqUnique(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ss, err := OpenSessionStore(SessionStoreOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()

	sdb, err := ss.Acquire(ctx, "sess-seq")
	if err != nil {
		t.Fatal(err)
	}
	ins := func(id string, seq int) error {
		_, err := sdb.conn().ExecContext(ctx,
			`INSERT INTO messages(id, seq, role, content, created_at) VALUES(?,?,?,?,?)`,
			id, seq, "user", "hi", NowMillis())
		return err
	}
	if err := ins("m1", 1); err != nil {
		t.Fatal(err)
	}
	if err := ins("m2", 1); err == nil {
		t.Fatal("重复 seq 应被唯一索引拒绝")
	}
	if err := ins("m3", 2); err != nil {
		t.Fatalf("不同 seq 应可插入: %v", err)
	}
}
