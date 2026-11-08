package logvault

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"time"

	"harness-go/internal/observ"
)

// redactPayload 委托 observ 做凭据脱敏（此处包一层，便于单体测试替换）。
func redactPayload(s string) string { return observ.RedactPayload(s) }

// sha256Hex 计算 sha256 十六进制串。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// newID 生成 16 字节随机十六进制 ID（日志行主键，无需 UUID 格式）。
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "fallback-" + hex.EncodeToString([]byte(time.Now().Format("20060102150405.000000")))
	}
	return hex.EncodeToString(b[:])
}

// nullIfEmpty 空串转为 NULL（避免 '' 与 NULL 语义混淆）。
func nullIfEmpty(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// boolToInt SQLite 无 bool 类型，统一 0/1。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// upsertRollup 在一批写入的同一事务内更新小时预聚合。
//
// 为什么必须预聚合：30 天明细约 250 万行，SUM 需 1~2s；
// rollup 只有 720 小时 × 模型数（数千行），聚合 <20ms。
func upsertRollup(ctx context.Context, tx *sql.Tx, jobs []llmJob) error {
	type key struct {
		day      string
		hour     int
		model    string
		provider string
	}
	type agg struct {
		calls        int64
		prompt       int64
		completion   int64
		cached       int64
		estimatedCnt int64
	}
	buckets := make(map[key]*agg, len(jobs))
	for _, j := range jobs {
		if j.kind != "llm" {
			continue
		}
		t := time.UnixMilli(j.llm.CreatedAt)
		k := key{
			day:      t.Format(dayLayout),
			hour:     t.Hour(),
			model:    j.llm.Model,
			provider: j.llm.Provider,
		}
		a, ok := buckets[k]
		if !ok {
			a = &agg{}
			buckets[k] = a
		}
		a.calls++
		a.prompt += j.llm.PromptTokens
		a.completion += j.llm.CompletionTokens
		a.cached += j.llm.CachedTokens
		if j.llm.IsEstimated {
			a.estimatedCnt++
		}
	}
	for k, a := range buckets {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO rollup_hourly(day, hour, model, provider, calls, prompt_tokens,
			                          completion_tokens, cached_tokens, estimated_calls)
			VALUES(?,?,?,?,?,?,?,?,?)
			ON CONFLICT(day, hour, model, provider) DO UPDATE SET
				calls             = calls + excluded.calls,
				prompt_tokens     = prompt_tokens + excluded.prompt_tokens,
				completion_tokens = completion_tokens + excluded.completion_tokens,
				cached_tokens     = cached_tokens + excluded.cached_tokens,
				estimated_calls   = estimated_calls + excluded.estimated_calls`,
			k.day, k.hour, k.model, k.provider, a.calls, a.prompt, a.completion, a.cached, a.estimatedCnt)
		if err != nil {
			return err
		}
	}
	return nil
}
