package logvault

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// ============ 写入数据模型 ============

// LLMRecord 一次模型调用的日志记录。
type LLMRecord struct {
	ID               string
	CreatedAt        int64 // Unix 毫秒
	SessionID        string
	RunID            string
	MessageID        string
	TraceID          string
	Model            string
	Provider         string
	Endpoint         string
	Payload          string // 原始报文（JSON）；按档位决定 preview / 外置 / 丢弃
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	IsEstimated      bool
	TTFTMs           int64
	DurationMs       int64
	TokensPerSec     float64
	Status           string
	ErrorCode        string
	ErrorMessage     string
}

// ToolCallRecord 一次工具调用的日志记录（会话库 tool_events 的查询副本）。
type ToolCallRecord struct {
	ID            string
	CreatedAt     int64
	SessionID     string
	RunID         string
	ToolEventID   string
	ToolName      string
	Namespace     string
	ParamsJSON    string
	ResultJSON    string
	Status        string
	Risk          string
	Error         string
	DurationMs    int64
}

// Writer 天分片日志写入管道。
//
// 设计要点（对齐 docs/基础服务设计.md:301「快照写入不阻塞 TTFT」）：
//   - Record 走**非阻塞** select 投递有界队列；队列满即丢弃并计数，绝不阻塞调用方；
//   - 单写协程按 BatchSize 或 FlushMS 批量提交单事务；
//   - 按 created_at 路由天库，连接由 Shard 的 LRU 缓存持有。
//
// 这是「日志写入进不了 Run 关键路径」的物理保证。
type Writer struct {
	shard  *Shard
	blobs  *BlobStore
	modes  *ModeProvider
	logger *slog.Logger

	queue     chan llmJob
	batchSize int
	flush     time.Duration

	dropped  atomic.Int64
	written  atomic.Int64
	blobsHit atomic.Int64

	// closeTimeout 关闭时排空队列的最长等待（默认 30s，可由 WriterOptions 覆盖）。
	closeTimeout time.Duration

	wg     sync.WaitGroup
	closed atomic.Bool
	once   sync.Once
}

// llmJob 队列元素（LLM 与工具共用一条管道，按 kind 区分）。
type llmJob struct {
	kind string // "llm" | "tool"
	llm  LLMRecord
	tool ToolCallRecord
}

// WriterOptions 写入管道参数。
type WriterOptions struct {
	Shard     *Shard
	Blobs     *BlobStore
	Modes     *ModeProvider
	Logger    *slog.Logger
	QueueCap  int
	BatchSize int
	FlushMs   int
	// CloseTimeout 关闭时排空队列的最长等待（0 = 30s）。
	CloseTimeout time.Duration
}

// NewWriter 创建并启动写入管道协程。
func NewWriter(opt WriterOptions) *Writer {
	if opt.QueueCap <= 0 {
		opt.QueueCap = 4096
	}
	if opt.BatchSize <= 0 {
		opt.BatchSize = 500
	}
	if opt.FlushMs <= 0 {
		opt.FlushMs = 200
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	w := &Writer{
		shard:        opt.Shard,
		blobs:        opt.Blobs,
		modes:        opt.Modes,
		logger:       opt.Logger,
		queue:        make(chan llmJob, opt.QueueCap),
		batchSize:    opt.BatchSize,
		flush:        time.Duration(opt.FlushMs) * time.Millisecond,
		closeTimeout: opt.CloseTimeout,
	}
	if w.closeTimeout <= 0 {
		w.closeTimeout = 30 * time.Second
	}
	w.wg.Add(1)
	go w.run()
	return w
}

// RecordLLM 投递一条 LLM 记录（非阻塞；队列满丢弃并计数）。
func (w *Writer) RecordLLM(rec LLMRecord) {
	if w == nil || w.shard == nil {
		return
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = time.Now().UnixMilli()
	}
	if rec.ID == "" {
		rec.ID = newID()
	}
	if rec.Status == "" {
		rec.Status = "ok"
	}
	w.push(llmJob{kind: "llm", llm: rec})
}

// RecordToolCall 投递一条工具调用记录（非阻塞；可丢弃）。
func (w *Writer) RecordToolCall(rec ToolCallRecord) {
	if w == nil || w.shard == nil {
		return
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = time.Now().UnixMilli()
	}
	if rec.ID == "" {
		rec.ID = newID()
	}
	if rec.Status == "" {
		rec.Status = "ok"
	}
	w.push(llmJob{kind: "tool", tool: rec})
}

// push 非阻塞投递。
func (w *Writer) push(j llmJob) {
	select {
	case w.queue <- j:
	default:
		// 队列满：丢弃而不是阻塞。日志永远让位于业务。
		if n := w.dropped.Add(1); n%1000 == 1 {
			w.logger.Warn("日志队列已满，丢弃记录（不影响业务）", "dropped_total", n)
		}
	}
}

// run 单写协程：积攒批量或按超时提交。
func (w *Writer) run() {
	defer w.wg.Done()
	buf := make([]llmJob, 0, w.batchSize)
	timer := time.NewTimer(w.flush)
	defer timer.Stop()

	flushBatch := func() {
		if len(buf) == 0 {
			return
		}
		w.commit(buf)
		buf = buf[:0]
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(w.flush)
	}

	for {
		select {
		case j, ok := <-w.queue:
			if !ok {
				flushBatch()
				return
			}
			buf = append(buf, j)
			if len(buf) >= w.batchSize {
				flushBatch()
			}
		case <-timer.C:
			flushBatch()
		}
	}
}

// commit 按天分组后逐天库批量提交（每批一个事务）。
func (w *Writer) commit(batch []llmJob) {
	byDay := make(map[string][]llmJob, 2)
	for _, j := range batch {
		var ts int64
		if j.kind == "llm" {
			ts = j.llm.CreatedAt
		} else {
			ts = j.tool.CreatedAt
		}
		day := DayOf(ts)
		byDay[day] = append(byDay[day], j)
	}
	for day, jobs := range byDay {
		if err := w.commitDay(day, jobs); err != nil {
			w.logger.Warn("日志批量写入失败（已丢弃该批）", "day", day, "count", len(jobs), "err", err)
			continue
		}
		w.written.Add(int64(len(jobs)))
	}
}

// commitDay 在单个天库内用一个事务写入一批。
func (w *Writer) commitDay(day string, jobs []llmJob) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := w.shard.Day(ctx, day)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	mode := w.modes.SnapshotMode()
	for _, j := range jobs {
		if j.kind == "llm" {
			if err := w.insertLLM(ctx, tx, day, mode, j.llm); err != nil {
				return err
			}
			continue
		}
		if err := insertTool(ctx, tx, j.tool); err != nil {
			return err
		}
	}
	if err := upsertRollup(ctx, tx, jobs); err != nil {
		return err
	}
	return tx.Commit()
}

// insertLLM 按档位决定报文落盘方式后插入一行。
//
// off     → 不写 payload_*（只留元数据）
// preview → 写脱敏 preview + sha256 + bytes，正文不落盘
// full    → ≤8KB 内联；>8KB 外置 blob；两种都写 preview + sha256 + bytes
func (w *Writer) insertLLM(ctx context.Context, tx *sql.Tx, day string, mode SnapshotMode, rec LLMRecord) error {
	var preview, inline, sha sql.NullString
	var bytes int64

	if mode.WantsPayload() && rec.Payload != "" {
		// 脱敏必须在算 hash **之前**：日志里永远不出现原始凭据。
		// 代价是 hash 对应「脱敏后正文」，回读时能自校验一致性。
		safe := redactPayload(rec.Payload)
		bytes = int64(len(safe))
		preview = sql.NullString{String: truncatePreview(safe, previewLimit), Valid: true}

		if mode.WantsBlob() {
			if len(safe) <= inlineThreshold {
				inline = sql.NullString{String: safe, Valid: true}
			} else if w.blobs != nil {
				h, _, err := w.blobs.Put(day, []byte(safe))
				if err != nil {
					// 外置失败不阻断入库：退化为内联，保证日志不丢
					w.logger.Warn("报文正文外置失败，退化为内联", "err", err)
					inline = sql.NullString{String: safe, Valid: true}
				} else {
					sha = sql.NullString{String: h, Valid: true}
					w.blobsHit.Add(1)
				}
			}
		} else {
			// preview 档：只记 hash 便于事后核对，不落正文
			sha = sql.NullString{String: sha256Hex(safe), Valid: true}
		}
	}

	_, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO llm_requests(
			id, created_at, session_id, run_id, message_id, trace_id, model, provider, endpoint,
			payload_preview, payload_inline, payload_sha256, payload_bytes,
			prompt_tokens, completion_tokens, cached_tokens, is_estimated,
			ttft_ms, duration_ms, tokens_per_sec, status, error_code, error_message)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.CreatedAt, rec.SessionID, rec.RunID, rec.MessageID, rec.TraceID,
		rec.Model, rec.Provider, rec.Endpoint,
		preview, inline, sha, bytes,
		rec.PromptTokens, rec.CompletionTokens, rec.CachedTokens, boolToInt(rec.IsEstimated),
		rec.TTFTMs, rec.DurationMs, rec.TokensPerSec, rec.Status,
		nullIfEmpty(rec.ErrorCode), nullIfEmpty(rec.ErrorMessage))
	return err
}

// insertTool 插入一条工具调用副本（preview 截断，不含正文 blob）。
func insertTool(ctx context.Context, tx *sql.Tx, rec ToolCallRecord) error {
	params := truncatePreview(redactPayload(rec.ParamsJSON), previewLimit)
	result := truncatePreview(redactPayload(rec.ResultJSON), previewLimit)
	bytes := int64(len(rec.ParamsJSON) + len(rec.ResultJSON))
	_, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO tool_call_logs(
			id, created_at, session_id, run_id, tool_event_id, tool_name, namespace,
			params_preview, result_preview, bytes, status, risk, error, duration_ms)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.CreatedAt, rec.SessionID, rec.RunID, rec.ToolEventID, rec.ToolName, rec.Namespace,
		nullIfEmpty(params), nullIfEmpty(result), bytes, rec.Status, rec.Risk,
		nullIfEmpty(rec.Error), rec.DurationMs)
	return err
}

// Flush 等待队列排空（优雅关闭时调用）。
func (w *Writer) Flush(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close 关闭管道：停止接收、排空队列、等写协程退出。
//
// 超时语义（重要）：若队列积压过多，排空可能超过 CloseTimeout；
// 此时返回错误但**写协程不泄漏** —— 它会继续在后台排空并自行退出，
// 调用方应在返回错误后仍等待 Shard.Close 之前的时间窗口（main 中靠进程退出兜底）。
//
// 为什么需要可配置：full 档 + 队列打满时，6 万条积压的排空远超 10 秒，
// 硬编码 10s 会让优雅关闭变成「报错 + 文件句柄仍被占用」（Windows 上删不掉目录）。
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.once.Do(func() {
		w.closed.Store(true)
		close(w.queue)
	})
	timeout := w.closeTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return w.Flush(ctx)
}

// Dropped 返回累计丢弃条数（队列满时递增）。
func (w *Writer) Dropped() int64 {
	if w == nil {
		return 0
	}
	return w.dropped.Load()
}

// Written 返回累计成功写入条数。
func (w *Writer) Written() int64 {
	if w == nil {
		return 0
	}
	return w.written.Load()
}
