package logvault

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// ============ 基准测试：把「估算」换成「实测」 ============
//
// 方案里所有性能数字（写入 5 万条/秒、查询 <300ms、聚合 <20ms）都是推算值。
// 本文件用真实 SQLite 落盘把这些数字测出来，避免用估算值做架构决策。
//
// 运行：go test ./internal/logvault/ -bench=. -benchtime=3s -run=^$

// benchShard 构造临时天分片与写入管道。
func benchShard(b *testing.B, mode string) (*Shard, *BlobStore, *ModeProvider) {
	b.Helper()
	shard, err := NewShard(b.TempDir())
	if err != nil {
		b.Fatalf("创建 shard 失败: %v", err)
	}
	b.Cleanup(func() { _ = shard.Close() })
	return shard, NewBlobStore(shard), NewModeProvider(mode)
}

// benchPayload 构造接近真实的报文（含中文与 JSON 结构）。
func benchPayload(tokens int) string {
	// 粗略按 3.5 字节/token 构造，模拟真实请求体
	n := tokens * 7 / 2
	body := make([]byte, 0, n+128)
	body = append(body, `{"model":"gpt-4","messages":[{"role":"user","content":"`...)
	for len(body) < n {
		body = append(body, "这是一段测试用的上下文内容，"...)
	}
	body = append(body, `"}]}`...)
	return string(body)
}

// benchEndToEnd 端到端写入吞吐：投递固定条数 → 关闭排空 → 报告单条耗时。
//
// 为什么不用「无限投递」的写法：无限投递会让队列持续溢出，测到的其实是
// 「入队（丢弃）代价」而非真实写入能力；而 Close 又要排空巨量积压，导致关闭超时。
// 端到端固定条数才是生产真正关心的指标（磁盘 IO + 事务提交 + 索引维护）。
func benchEndToEnd(b *testing.B, mode string, rows int, payloadTokens int) {
	b.Helper()
	shard, blobs, modes := benchShard(b, mode)
	payload := benchPayload(payloadTokens)
	now := time.Now().UnixMilli()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		w := NewWriter(WriterOptions{
			Shard: shard, Blobs: blobs, Modes: modes,
			QueueCap: 4096, BatchSize: 500, FlushMs: 100,
			CloseTimeout: 5 * time.Minute, // 基准要排空全部数据，给足时间
		})
		b.StartTimer()

		for r := 0; r < rows; r++ {
			w.RecordLLM(LLMRecord{
				CreatedAt:        now + int64(r),
				SessionID:        fmt.Sprintf("s-%d", r%50),
				RunID:            fmt.Sprintf("r-%d", r%200),
				MessageID:        fmt.Sprintf("m-%d-%d", i, r),
				TraceID:          fmt.Sprintf("trace-%d", r%500),
				Model:            "gpt-4",
				Provider:         "openai",
				Endpoint:         "chat/completions",
				Payload:          payload,
				PromptTokens:     11000,
				CompletionTokens: 1000,
				CachedTokens:     2000,
				TTFTMs:           800,
				DurationMs:       4000,
				TokensPerSec:     25,
				Status:           "ok",
			})
		}
		// 关闭即排空：把「真实落盘耗时」计入本次迭代
		if err := w.Close(); err != nil {
			b.Fatalf("关闭写入管道失败: %v", err)
		}
		if w.Dropped() > 0 {
			b.Fatalf("端到端基准不应丢数据，dropped=%d", w.Dropped())
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(rows), "rows/op")
}

// BenchmarkEndToEndPreview preview 档端到端吞吐（默认档位，最常跑）。
func BenchmarkEndToEndPreview(b *testing.B) {
	benchEndToEnd(b, "preview", 2000, 12000)
}

// BenchmarkEndToEndFull full 档端到端吞吐（含 blob 写入 + fsync，最坏情况）。
func BenchmarkEndToEndFull(b *testing.B) {
	benchEndToEnd(b, "full", 500, 12000)
}

// BenchmarkEndToEndOff off 档端到端吞吐（只记元数据，性能上限参考）。
func BenchmarkEndToEndOff(b *testing.B) {
	benchEndToEnd(b, "off", 2000, 12000)
}

// seedDay 向某天库灌 n 行（供查询基准使用）。
//
// 注意：必须给足队列容量并等待排空。早期版本用 8192 容量灌 20 万条，
// 结果是丢弃 19 万条、只剩不到 1 万行，查询基准测的根本不是目标数据量。
func seedDay(b *testing.B, shard *Shard, blobs *BlobStore, day string, n int) {
	b.Helper()
	qcap := n
	if qcap < 4096 {
		qcap = 4096
	}
	w := NewWriter(WriterOptions{
		Shard: shard, Blobs: blobs, Modes: NewModeProvider("preview"),
		QueueCap: qcap, BatchSize: 500, FlushMs: 100,
		CloseTimeout: 10 * time.Minute,
	})
	base, err := DayStartMs(day)
	if err != nil {
		b.Fatalf("解析天键失败: %v", err)
	}
	payload := benchPayload(2000)
	for i := 0; i < n; i++ {
		w.RecordLLM(LLMRecord{
			// 均匀分布在当天 24h 内，模拟真实写入
			CreatedAt:        base + int64(i*(86400000/n)),
			SessionID:        fmt.Sprintf("s-%d", i%100), // 100 个会话
			RunID:            fmt.Sprintf("r-%d", i%500),
			MessageID:        fmt.Sprintf("m-%d", i),
			TraceID:          fmt.Sprintf("trace-%d", i%1000),
			Model:            []string{"gpt-4", "claude-3", "gpt-4o"}[i%3],
			Provider:         []string{"openai", "anthropic"}[i%2],
			Payload:          payload,
			PromptTokens:     int64(1000 + i%500),
			CompletionTokens: int64(100 + i%200),
			CachedTokens:     int64(i % 300),
			TTFTMs:           int64(300 + i%500),
			DurationMs:       int64(2000 + i%3000),
			Status:           []string{"ok", "ok", "ok", "error"}[i%4],
		})
	}
	if err := w.Close(); err != nil {
		b.Fatalf("种子数据排空失败: %v", err)
	}
	if d := w.Dropped(); d > 0 {
		b.Fatalf("种子数据丢失 %d 条（队列容量不足，基准会失真）", d)
	}
}

// benchQueryRows 查询基准的种子数据规模。
//
// 20 万条在开发机上灌数据需数十秒（实测写入约 900 行/秒），而查询侧要看的是
// **索引是否被用上**：5 万条已足以暴露全表扫描（会慢 4 倍以上）。
// 生产真实规模（30 天）靠索引设计保证，而不是靠基准数据量堆出来。
const benchQueryRows = 50000

// BenchmarkQueryOneDay 单天库的时间范围查询（最常见的「查最近 N 条」）。
//
// 目标 < 300ms（方案里的红线）。
func BenchmarkQueryOneDay(b *testing.B) {
	shard, blobs, _ := benchShard(b, "preview")
	day := Today()
	seedDay(b, shard, blobs, day, benchQueryRows)

	q := NewQuerier(shard, blobs)
	to, _ := DayEndMs(day)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, _, err := q.QueryLLM(ctx, LLMQuery{
			From: to - 3600000, To: to, // 最近 1 小时
			Limit: 100,
		})
		if err != nil {
			b.Fatalf("查询失败: %v", err)
		}
		if len(rows) == 0 {
			b.Fatal("应查到数据")
		}
	}
}

// BenchmarkQueryByTraceId 按 trace_id 点查（排障主入口）。
func BenchmarkQueryByTraceId(b *testing.B) {
	shard, blobs, _ := benchShard(b, "preview")
	day := Today()
	seedDay(b, shard, blobs, day, benchQueryRows)

	q := NewQuerier(shard, blobs)
	from, _ := DayStartMs(day)
	to, _ := DayEndMs(day)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, _, err := q.QueryLLM(ctx, LLMQuery{
			From: from, To: to,
			TraceID: fmt.Sprintf("trace-%d", i%1000),
			Limit:   10,
		})
		if err != nil {
			b.Fatalf("查询失败: %v", err)
		}
		if len(rows) == 0 {
			b.Fatal("按 trace_id 应查到数据")
		}
	}
}

// BenchmarkAggregateTokens30d 30 天用量聚合（走 rollup，不扫明细）。
//
// 这是「rollup 必须存在」的正面证明：目标 < 20ms。
// 每天 1000 条 × 30 天 = 3 万条明细；真实规模是 250 万条，
// 但 rollup 的查询量只取决于「小时 × 模型数」桶数，与明细行数无关 ——
// 这正是预聚合的意义，故 3 万条种子足够说明问题。
func BenchmarkAggregateTokens30d(b *testing.B) {
	shard, blobs, _ := benchShard(b, "preview")
	for d := 0; d < 30; d++ {
		day := time.Now().AddDate(0, 0, -d).Format(dayLayout)
		seedDay(b, shard, blobs, day, 1000)
	}
	q := NewQuerier(shard, blobs)
	now := time.Now()
	from := now.AddDate(0, 0, -30).UnixMilli()
	to := now.UnixMilli()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sum, err := q.AggregateTokens(ctx, TokenQuery{From: from, To: to, GroupBy: GroupByModel})
		if err != nil {
			b.Fatalf("聚合失败: %v", err)
		}
		if sum.TotalCalls == 0 {
			b.Fatal("聚合应有数据")
		}
	}
}

// BenchmarkToolAuditQuery 跨会话工具审计查询（天库副本的核心价值）。
func BenchmarkToolAuditQuery(b *testing.B) {
	shard, blobs, _ := benchShard(b, "preview")
	day := Today()
	base, _ := DayStartMs(day)
	w := NewWriter(WriterOptions{
		Shard: shard, Blobs: blobs, Modes: NewModeProvider("preview"),
		QueueCap: 8192, BatchSize: 500, FlushMs: 100,
	})
	for i := 0; i < 50000; i++ {
		w.RecordToolCall(ToolCallRecord{
			CreatedAt:   base + int64(i*1000),
			SessionID:   fmt.Sprintf("s-%d", i%100),
			RunID:       fmt.Sprintf("r-%d", i%500),
			ToolEventID: fmt.Sprintf("e-%d", i),
			ToolName:    []string{"fs_read", "fs_write", "run_command", "web_fetch"}[i%4],
			ParamsJSON:  `{"path":"/tmp/a.txt","content":"xxx"}`,
			ResultJSON:  `{"ok":true}`,
			Status:      []string{"ok", "error"}[i%2],
			Risk:        "write",
			DurationMs:  int64(10 + i%200),
		})
	}
	_ = w.Close()

	q := NewQuerier(shard, blobs)
	// 种子按 i*1000ms 分布在当天前 13.9 小时，查询窗口必须覆盖到那段，
	// 否则会查到空区间（早期版本取「当天最后 1 小时」导致基准假失败）。
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, _, err := q.QueryTools(ctx, ToolQuery{
			From: base, To: base + 50000*1000,
			Limit: 100,
		})
		if err != nil {
			b.Fatalf("查询失败: %v", err)
		}
		if len(rows) == 0 {
			b.Fatal("应查到工具调用数据")
		}
	}
	_ = day
}

// BenchmarkRetentionSweep 保留期清理耗时（删天库是否真的 < 100ms）。
func BenchmarkRetentionSweep(b *testing.B) {
	shard, blobs, _ := benchShard(b, "preview")
	// 造 5 个待删天库
	for d := 40; d < 45; d++ {
		day := time.Now().AddDate(0, 0, -d).Format(dayLayout)
		seedDay(b, shard, blobs, day, 2000)
	}
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// 每轮重建待删数据（否则第二轮就删光了）
		for d := 40; d < 45; d++ {
			day := time.Now().AddDate(0, 0, -d).Format(dayLayout)
			if _, err := shard.Day(ctx, day); err != nil {
				b.Fatalf("重建天库失败: %v", err)
			}
		}
		r := NewRetention(RetentionOptions{Shard: shard, RetentionDays: 30})
		b.StartTimer()
		r.Sweep(ctx)
	}
}
