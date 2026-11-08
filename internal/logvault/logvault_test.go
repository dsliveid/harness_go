package logvault

import (
	"context"
	"testing"
	"time"
)

// 天键是「文件名即时间索引」的基础，格式错一天就会跑去查错库。
func TestDayKeyRoundTrip(t *testing.T) {
	now := time.Now()
	day := DayOfTime(now)
	if len(day) != 8 {
		t.Fatalf("天键必须是 8 位 YYYYMMDD，得到 %q", day)
	}
	start, err := DayStartMs(day)
	if err != nil {
		t.Fatalf("解析天键失败: %v", err)
	}
	end, err := DayEndMs(day)
	if err != nil {
		t.Fatalf("解析天键失败: %v", err)
	}
	if end <= start {
		t.Fatalf("日终必须晚于日初: start=%d end=%d", start, end)
	}
	// 跨度必须是 24h - 1ms
	if want := int64(24*time.Hour/time.Millisecond - 1); end-start != want {
		t.Fatalf("日跨度应为 %d ms，得到 %d", want, end-start)
	}
	// ms → 天键 与 time → 天键 必须一致
	if DayOf(start) != day {
		t.Fatalf("DayOf(DayStartMs(day)) 应等于 %s，得到 %s", day, DayOf(start))
	}
	// 非法天键必须报错，而不是静默返回零值
	if _, err := ParseDay("2026-11-08"); err == nil {
		t.Fatal("非法天键应报错")
	}
}

// daysInRange 决定「一次查询要开几个天库」，多开就是白付打开成本。
func TestDaysInRange(t *testing.T) {
	day1 := time.Date(2026, 11, 8, 10, 0, 0, 0, time.Local)
	day2 := time.Date(2026, 11, 9, 10, 0, 0, 0, time.Local)

	// 同一天内：只 1 个
	got := daysInRange(day1.UnixMilli(), day1.Add(time.Hour).UnixMilli())
	if len(got) != 1 || got[0] != "20261108" {
		t.Fatalf("同天范围应只返回 1 个天键，得到 %v", got)
	}

	// 跨天：2 个，且升序
	got = daysInRange(day1.UnixMilli(), day2.UnixMilli())
	if len(got) != 2 || got[0] != "20261108" || got[1] != "20261109" {
		t.Fatalf("跨天范围应返回升序 2 个天键，得到 %v", got)
	}

	// 反向范围：空（不 panic）
	if got = daysInRange(day2.UnixMilli(), day1.UnixMilli()); len(got) != 0 {
		t.Fatalf("反向范围应返回空，得到 %v", got)
	}
}

// ModeProvider 是写入热路径上被每次调用读取的，必须：零值可用、非法值回退 preview、
// Refresh 立即生效（否则用户切了档位要重启才生效）。
func TestModeProvider(t *testing.T) {
	var zero ModeProvider
	if zero.SnapshotMode() != ModePreview {
		t.Fatalf("零值应回退 preview，得到 %s", zero.SnapshotMode())
	}
	if zero.SnapshotMode().WantsPayload() != true || zero.SnapshotMode().WantsBlob() != false {
		t.Fatal("preview 应要 payload 但不要 blob")
	}

	p := NewModeProvider("full")
	if p.SnapshotMode() != ModeFull {
		t.Fatalf("初始应为 full，得到 %s", p.SnapshotMode())
	}
	if !p.SnapshotMode().WantsBlob() {
		t.Fatal("full 应要 blob")
	}

	// 非法值不得让写入路径崩：归一为 preview
	p.Refresh("GARBAGE")
	if p.SnapshotMode() != ModePreview {
		t.Fatalf("非法值应归一为 preview，得到 %s", p.SnapshotMode())
	}

	p.Refresh("off")
	if p.SnapshotMode() != ModeOff {
		t.Fatalf("应为 off，得到 %s", p.SnapshotMode())
	}
	if p.SnapshotMode().WantsPayload() {
		t.Fatal("off 不应写 payload")
	}

	// nil 接收者安全（引擎未注入 Traces 时相关路径全 nil）
	var nilP *ModeProvider
	if nilP.SnapshotMode() != ModePreview {
		t.Fatal("nil provider 应回退 preview")
	}
	nilP.Refresh("full") // 不得 panic
}

// Normalize 大小写与空白容错（settings 值来自人工输入，不能假定格式规整）。
func TestNormalize(t *testing.T) {
	cases := map[string]SnapshotMode{
		"FULL":    ModeFull,
		" full ":  ModeFull,
		"Preview": ModePreview,
		"OFF":     ModeOff,
		"":        ModePreview,
		"xxx":     ModePreview,
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Fatalf("Normalize(%q) = %s，期望 %s", in, got, want)
		}
	}
}

// 天库创建 + 写入 + 查询的端到端最小闭环（验证 schema 与索引真的生效）。
func TestShardWriteAndQuery(t *testing.T) {
	shard, err := NewShard(t.TempDir())
	if err != nil {
		t.Fatalf("创建 shard 失败: %v", err)
	}
	defer func() { _ = shard.Close() }()
	blobs := NewBlobStore(shard)
	modes := NewModeProvider("preview")
	ctx := context.Background()

	w := NewWriter(WriterOptions{Shard: shard, Blobs: blobs, Modes: modes, QueueCap: 32, BatchSize: 4, FlushMs: 10})
	now := time.Now().UnixMilli()
	for i := 0; i < 4; i++ {
		w.RecordLLM(LLMRecord{
			CreatedAt: now + int64(i),
			SessionID: "s1",
			RunID:     "r1",
			TraceID:   "trace-abc",
			Model:     "m1",
			Provider:  "p1",
			Payload:   `{"password":"hunter2","msg":"hello"}`,
			PromptTokens: 10, CompletionTokens: 5, CachedTokens: 2,
			TTFTMs: 120, DurationMs: 800, TokensPerSec: 6.25,
			Status: "ok",
		})
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭写入管道失败: %v", err)
	}
	if w.Written() == 0 {
		t.Fatal("应有写入成功计数")
	}

	q := NewQuerier(shard, blobs)
	rows, total, err := q.QueryLLM(ctx, LLMQuery{From: now - 1000, To: now + 10000, SessionID: "s1"})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if total != 4 || len(rows) != 4 {
		t.Fatalf("应查到 4 行，得到 total=%d len=%d", total, len(rows))
	}
	// preview 档：必须有脱敏预览，且**不得**含明文密码
	if rows[0].PayloadPreview == "" {
		t.Fatal("preview 档应有预览")
	}
	if containsStr(rows[0].PayloadPreview, "hunter2") {
		t.Fatalf("预览未脱敏: %s", rows[0].PayloadPreview)
	}
	// preview 档无正文
	payload, err := q.GetPayload(ctx, rows[0].ID, rows[0].CreatedAt)
	if err != nil {
		t.Fatalf("取正文失败: %v", err)
	}
	if payload != "" {
		t.Fatalf("preview 档不应有正文，得到 %q", payload)
	}

	// 按 trace_id 检索（排障主入口）
	rows, _, err = q.QueryLLM(ctx, LLMQuery{From: now - 1000, To: now + 10000, TraceID: "trace-abc"})
	if err != nil || len(rows) != 4 {
		t.Fatalf("按 trace_id 应查到 4 行，得到 %d err=%v", len(rows), err)
	}

	// rollup 应已累加
	sum, err := q.AggregateTokens(ctx, TokenQuery{From: now - 1000, To: now + 10000})
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	if sum.TotalCalls != 4 || sum.TotalPrompt != 40 || sum.TotalCompletion != 20 {
		t.Fatalf("聚合结果异常: %+v", sum)
	}
}

// full 档：≤8KB 走内联，>8KB 外置 blob，且两者都能取回原文。
func TestWriterFullModeInlineAndBlob(t *testing.T) {
	shard, err := NewShard(t.TempDir())
	if err != nil {
		t.Fatalf("创建 shard 失败: %v", err)
	}
	defer func() { _ = shard.Close() }()
	blobs := NewBlobStore(shard)
	w := NewWriter(WriterOptions{Shard: shard, Blobs: blobs, Modes: NewModeProvider("full"), QueueCap: 16, BatchSize: 2, FlushMs: 10})
	ctx := context.Background()
	now := time.Now().UnixMilli()

	small := `{"msg":"small payload"}`
	big := `{"msg":"` + repeatStr("x", 20000) + `"}`
	w.RecordLLM(LLMRecord{CreatedAt: now, SessionID: "s1", Payload: small, Status: "ok"})
	w.RecordLLM(LLMRecord{CreatedAt: now + 1, SessionID: "s1", Payload: big, Status: "ok"})
	if err := w.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	q := NewQuerier(shard, blobs)
	rows, _, err := q.QueryLLM(ctx, LLMQuery{From: now - 1000, To: now + 10000, SessionID: "s1", Limit: 10})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("应查到 2 行，得到 %d", len(rows))
	}
	// 两行都必须可取回正文（一行 inline，一行 blob）
	for _, r := range rows {
		got, pErr := q.GetPayload(ctx, r.ID, r.CreatedAt)
		if pErr != nil {
			t.Fatalf("取正文失败: %v", pErr)
		}
		if got == "" {
			t.Fatalf("full 档应能取回正文，行 %s 为空", r.ID)
		}
		if !r.PayloadAvailable {
			t.Fatalf("full 档 PayloadAvailable 应为 true，行 %s", r.ID)
		}
	}
}

// off 档：不写任何报文内容，但元数据（token/ttft）仍要记全，否则统计会缺行。
func TestWriterOffMode(t *testing.T) {
	shard, err := NewShard(t.TempDir())
	if err != nil {
		t.Fatalf("创建 shard 失败: %v", err)
	}
	defer func() { _ = shard.Close() }()
	blobs := NewBlobStore(shard)
	w := NewWriter(WriterOptions{Shard: shard, Blobs: blobs, Modes: NewModeProvider("off"), QueueCap: 8, BatchSize: 1, FlushMs: 5})
	ctx := context.Background()
	now := time.Now().UnixMilli()

	w.RecordLLM(LLMRecord{
		CreatedAt: now, SessionID: "s1", Payload: `{"secret":"x"}`,
		PromptTokens: 7, CompletionTokens: 3, TTFTMs: 50, Status: "ok",
	})
	if err := w.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	q := NewQuerier(shard, blobs)
	rows, _, err := q.QueryLLM(ctx, LLMQuery{From: now - 1000, To: now + 10000, SessionID: "s1"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("应查到 1 行，得到 %d err=%v", len(rows), err)
	}
	if rows[0].PayloadPreview != "" {
		t.Fatalf("off 档不应有预览，得到 %q", rows[0].PayloadPreview)
	}
	if rows[0].PayloadBytes != 0 {
		t.Fatalf("off 档 payload_bytes 应为 0，得到 %d", rows[0].PayloadBytes)
	}
	// 元数据必须完整
	if rows[0].PromptTokens != 7 || rows[0].CompletionTokens != 3 || rows[0].TTFTMs != 50 {
		t.Fatalf("off 档元数据不完整: %+v", rows[0])
	}
}

// 队列满时必须丢弃而不是阻塞（日志永远让位于业务）。
func TestWriterQueueFullDoesNotBlock(t *testing.T) {
	shard, err := NewShard(t.TempDir())
	if err != nil {
		t.Fatalf("创建 shard 失败: %v", err)
	}
	defer func() { _ = shard.Close() }()
	// QueueCap=1 + 不启动消费（BatchSize 极大、FlushMs 极大）来制造「写不进去」
	w := NewWriter(WriterOptions{
		Shard: shard, Blobs: NewBlobStore(shard), Modes: NewModeProvider("preview"),
		QueueCap: 1, BatchSize: 1 << 20, FlushMs: 60000,
	})
	now := time.Now().UnixMilli()
	// 大量投递必须立即返回（若阻塞，本测试会超时）
	for i := 0; i < 500; i++ {
		w.RecordLLM(LLMRecord{CreatedAt: now, SessionID: "s1", Status: "ok"})
	}
	if w.Dropped() == 0 {
		t.Fatal("队列打满后应有丢弃计数")
	}
	_ = w.Close()
}

// tool_call_logs 是会话库 tool_events 的查询副本，必须支持跨会话按时间检索。
func TestToolCallLogsQuery(t *testing.T) {
	shard, err := NewShard(t.TempDir())
	if err != nil {
		t.Fatalf("创建 shard 失败: %v", err)
	}
	defer func() { _ = shard.Close() }()
	blobs := NewBlobStore(shard)
	w := NewWriter(WriterOptions{Shard: shard, Blobs: blobs, Modes: NewModeProvider("preview"), QueueCap: 32, BatchSize: 4, FlushMs: 10})
	ctx := context.Background()
	now := time.Now().UnixMilli()

	w.RecordToolCall(ToolCallRecord{
		CreatedAt: now, SessionID: "s1", RunID: "r1", ToolEventID: "e1",
		ToolName: "fs_write", ParamsJSON: `{"path":"/a","password":"hunter2"}`,
		ResultJSON: `{"ok":true}`, Status: "ok", Risk: "write", DurationMs: 30,
	})
	w.RecordToolCall(ToolCallRecord{
		CreatedAt: now + 1, SessionID: "s2", RunID: "r2", ToolEventID: "e2",
		ToolName: "fs_read", ParamsJSON: `{"path":"/b"}`, Status: "error",
		Risk: "readonly", Error: "not found",
	})
	if err := w.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	q := NewQuerier(shard, blobs)
	// 跨会话（不指定 session_id）：应看到 2 条
	rows, total, err := q.QueryTools(ctx, ToolQuery{From: now - 1000, To: now + 10000})
	if err != nil || total != 2 {
		t.Fatalf("跨会话应查到 2 条，得到 %d err=%v", total, err)
	}
	// 参数预览必须脱敏
	for _, r := range rows {
		if containsStr(r.ParamsPreview, "hunter2") {
			t.Fatalf("工具参数预览未脱敏: %s", r.ParamsPreview)
		}
	}
	// 按工具名过滤
	rows, _, err = q.QueryTools(ctx, ToolQuery{From: now - 1000, To: now + 10000, ToolName: "fs_read"})
	if err != nil || len(rows) != 1 || rows[0].ToolName != "fs_read" {
		t.Fatalf("按工具名过滤失败: %d err=%v", len(rows), err)
	}
	// 按状态过滤
	rows, _, err = q.QueryTools(ctx, ToolQuery{From: now - 1000, To: now + 10000, Status: "error"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("按状态过滤失败: %d err=%v", len(rows), err)
	}
}

// 保留期清理是「磁盘有界」的唯一保证，必须验证：
//  1. 超期天库被物理删除；
//  2. 未超期天库保留；
//  3. AvailableRange 随之前移（界面靠它明示「已清理」）。
func TestRetentionSweepDeletesExpired(t *testing.T) {
	shard, err := NewShard(t.TempDir())
	if err != nil {
		t.Fatalf("创建 shard 失败: %v", err)
	}
	defer func() { _ = shard.Close() }()
	ctx := context.Background()

	// 造三个天库：今天、40 天前、100 天前
	today := Today()
	old40 := time.Now().AddDate(0, 0, -40).Format(dayLayout)
	old100 := time.Now().AddDate(0, 0, -100).Format(dayLayout)
	for _, d := range []string{today, old40, old100} {
		if _, err := shard.Day(ctx, d); err != nil {
			t.Fatalf("创建天库 %s 失败: %v", d, err)
		}
	}
	if got := len(shard.ListDays()); got != 3 {
		t.Fatalf("应有 3 个天库，得到 %d", got)
	}

	r := NewRetention(RetentionOptions{Shard: shard, RetentionDays: 30})
	rep := r.Sweep(ctx)

	// 40 天前与 100 天前都必须被删
	days := shard.ListDays()
	if len(days) != 1 || days[0] != today {
		t.Fatalf("只应保留今天的天库，得到 %v", days)
	}
	if len(rep.DeletedDays) != 2 {
		t.Fatalf("应删除 2 个天库，得到 %v", rep.DeletedDays)
	}
	if rep.EarliestDay != today {
		t.Fatalf("earliest 应为今天 %s，得到 %s", today, rep.EarliestDay)
	}
}

// 配额熔断：即使天数未到，超配额也必须强删最早天库（full 档 30 天可达 150GB）。
func TestRetentionQuotaForced(t *testing.T) {
	shard, err := NewShard(t.TempDir())
	if err != nil {
		t.Fatalf("创建 shard 失败: %v", err)
	}
	defer func() { _ = shard.Close() }()
	ctx := context.Background()

	// 造 3 个「未超期」的天库，各写入一些数据让文件有实际大小
	modes := NewModeProvider("full")
	blobs := NewBlobStore(shard)
	for i := 1; i <= 3; i++ {
		d := time.Now().AddDate(0, 0, -i).Format(dayLayout)
		db, err := shard.Day(ctx, d)
		if err != nil {
			t.Fatalf("创建天库失败: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO llm_requests(id, created_at, session_id)
			VALUES(?,?,?)`, "id-"+d, time.Now().UnixMilli(), "s1"); err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}
	_ = modes
	_ = blobs
	before := len(shard.ListDays())
	if before != 3 {
		t.Fatalf("应有 3 个天库，得到 %d", before)
	}

	// 配额设为 1 字节 → 必定触发强删，直到只剩不到上限（最少剩 1 个就够验证行为）
	r := NewRetention(RetentionOptions{Shard: shard, RetentionDays: 3650, MaxTotalBytes: 1})
	rep := r.Sweep(ctx)
	if len(rep.QuotaForced) == 0 {
		t.Fatalf("应发生配额强制删除，报告: %+v", rep)
	}
	if len(shard.ListDays()) >= before {
		t.Fatalf("配额熔断后应减少天库，仍有 %d 个", len(shard.ListDays()))
	}
}

// ---------- 测试辅助 ----------

func containsStr(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexOfStr(s, sub) >= 0
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func repeatStr(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
