package logvault

import (
	"context"
	"database/sql"
	"fmt"
)

// ensureSchema 幂等建表 + 幂等补列，并按需提升 PRAGMA user_version。
//
// 天库演进纪律（与业务库不同，务必区分）：
//   - 业务库：//go:embed migrations + schema_migrations 严格版本化，已发布迁移永不修改；
//   - 天库：可丢弃数据，允许「CREATE TABLE IF NOT EXISTS + ALTER TABLE ADD COLUMN」
//     幂等补齐。历史天库 schema 落后不影响业务正确性，读取时缺列降级即可。
//
// 铁律：已存在的列绝不 DROP/RENAME —— 旧库可能仍在被查询。
func ensureSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("logvault: 建表失败: %w", err)
	}
	// 幂等补列（新版本新增列的兜底，失败不致命：读路径按缺列降级）
	for _, c := range pendingColumns {
		// 忽略「列已存在」错误：modernc sqlite 返回 duplicate column name
		_, _ = db.ExecContext(ctx, c)
	}
	// 版本号仅作记录，不参与分支判定（补列已幂等）
	_, _ = db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion))
	return nil
}

// pendingColumns 历史天库的幂等补列语句（每次打开都会尝试，已存在则忽略错误）。
//
// 何时往这里加：给已有表新增列时。新库由 ddl 直接建出，老库靠这里补。
var pendingColumns = []string{}

// ddl 天库完整 schema。
const ddl = `
-- ============ LLM 报文快照 ============
-- 一行 = 一次模型调用。payload 正文按抓取档位决定是否落盘：
--   off     → 不写 payload_* 任何内容，只留元数据
--   preview → 写 payload_preview（脱敏前 2KB）+ payload_sha256 + payload_bytes
--   full    → 额外把正文写 blob/<day>/<xx>/<sha256>，payload_sha256 指向它
CREATE TABLE IF NOT EXISTS llm_requests (
  id               TEXT PRIMARY KEY,
  created_at       INTEGER NOT NULL,               -- Unix 毫秒，第一索引列
  session_id       TEXT NOT NULL DEFAULT '',
  run_id           TEXT NOT NULL DEFAULT '',
  message_id       TEXT NOT NULL DEFAULT '',
  trace_id         TEXT NOT NULL DEFAULT '',
  model            TEXT NOT NULL DEFAULT '',
  provider         TEXT NOT NULL DEFAULT '',
  endpoint         TEXT NOT NULL DEFAULT '',       -- 实际请求的 API 路径
  payload_preview  TEXT,                           -- 脱敏后前 2KB；正文已外置
  payload_inline   TEXT,                           -- full 模式下 ≤8KB 的内联正文
  payload_sha256   TEXT,                           -- 原文 sha256（外置寻址 / 内联校验）
  payload_bytes    INTEGER NOT NULL DEFAULT 0,     -- 原文字节数
  prompt_tokens    INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens    INTEGER NOT NULL DEFAULT 0,
  is_estimated     INTEGER NOT NULL DEFAULT 0,     -- 1 = 用量为估算值
  ttft_ms          INTEGER NOT NULL DEFAULT 0,     -- 首 token 延迟
  duration_ms      INTEGER NOT NULL DEFAULT 0,     -- 整次调用耗时
  tokens_per_sec   REAL NOT NULL DEFAULT 0,
  status           TEXT NOT NULL DEFAULT 'ok',     -- ok | error | cancelled
  error_code       TEXT,
  error_message    TEXT
);

-- 时间范围是主查询维度（跨会话、跨天分片之后的第一层筛选）
CREATE INDEX IF NOT EXISTS idx_llm_requests_created ON llm_requests(created_at);
-- 会话内报文回放
CREATE INDEX IF NOT EXISTS idx_llm_requests_session ON llm_requests(session_id, created_at);
CREATE INDEX IF NOT EXISTS idx_llm_requests_run     ON llm_requests(run_id);
CREATE INDEX IF NOT EXISTS idx_llm_requests_trace   ON llm_requests(trace_id);
CREATE INDEX IF NOT EXISTS idx_llm_requests_message ON llm_requests(message_id);
-- 注：刻意不建 (model, created_at) 索引 —— 按模型聚合走 rollup_hourly，
-- 明细表少一个索引可降低写放大（8.3 万行/天 × 索引数）。

-- ============ 工具调用日志 ============
-- 这是会话库 tool_events 的**查询副本**（事实源仍在会话库）：
-- 会话库按 run_id 查（业务回放），天库按时间跨会话查（审计）。
-- 副本可丢弃：丢失只影响跨会话审计，不影响业务。
CREATE TABLE IF NOT EXISTS tool_call_logs (
  id             TEXT PRIMARY KEY,
  created_at     INTEGER NOT NULL,
  session_id     TEXT NOT NULL DEFAULT '',
  run_id         TEXT NOT NULL DEFAULT '',
  tool_event_id  TEXT NOT NULL DEFAULT '',
  tool_name      TEXT NOT NULL DEFAULT '',
  namespace      TEXT NOT NULL DEFAULT 'builtin',
  params_preview TEXT,                             -- 脱敏后参数预览（截断）
  result_preview TEXT,                             -- 脱敏后结果预览（截断）
  bytes          INTEGER NOT NULL DEFAULT 0,       -- 原始参数+结果总字节数
  status         TEXT NOT NULL DEFAULT 'ok',
  risk           TEXT NOT NULL DEFAULT 'readonly',
  error          TEXT,
  duration_ms    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_tool_call_logs_created ON tool_call_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_tool_call_logs_session ON tool_call_logs(session_id, created_at);
CREATE INDEX IF NOT EXISTS idx_tool_call_logs_tool    ON tool_call_logs(tool_name, created_at);
CREATE INDEX IF NOT EXISTS idx_tool_call_logs_status  ON tool_call_logs(status, created_at);
CREATE INDEX IF NOT EXISTS idx_tool_call_logs_run     ON tool_call_logs(run_id);

-- ============ 用量预聚合（小时 × 模型 × 厂商） ============
-- 为什么必须预聚合：30 天 250 万行明细上做 SUM 需 1~2s，
-- 而 720 小时 × 模型数 只有数千行，聚合 <20ms。
-- estimated_calls 显式记录估算占比 —— 绝不隐藏估算值。
CREATE TABLE IF NOT EXISTS rollup_hourly (
  day               TEXT NOT NULL,                 -- YYYYMMDD
  hour              INTEGER NOT NULL,              -- 0~23
  model             TEXT NOT NULL DEFAULT '',
  provider          TEXT NOT NULL DEFAULT '',
  calls             INTEGER NOT NULL DEFAULT 0,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens     INTEGER NOT NULL DEFAULT 0,
  estimated_calls   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(day, hour, model, provider)
);
`
