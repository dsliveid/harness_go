-- 会话库初始化（docs/数据存储方案.md v2.0 §2.2）
-- 每个会话一个库：data/sessions/<sid>/session.db
-- 会话库内不再有 sessions 表（避免与主库外键冲突）；session_meta 单行使库自包含。
-- 铁律：已发布的迁移文件永不修改，只能追加新文件。

-- ============ 会话自描述（单行） ============
CREATE TABLE IF NOT EXISTS session_meta (
  id         INTEGER PRIMARY KEY CHECK (id = 1),
  session_id TEXT NOT NULL,
  title      TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);

-- ============ 消息 ============
-- 分库后会话内唯一：UNIQUE(seq)（原单库方案为 UNIQUE(session_id, seq)）
CREATE TABLE IF NOT EXISTS messages (
  id               TEXT PRIMARY KEY,
  seq              INTEGER NOT NULL,
  run_id           TEXT,
  role             TEXT NOT NULL,                      -- system | user | assistant | tool
  content          TEXT NOT NULL DEFAULT '',           -- >32KB 时存 blobref:<hash>
  reasoning        TEXT,
  tool_calls_json  TEXT,
  tool_call_id     TEXT,
  attachments_json TEXT,                               -- [{hash,mime,size,name}]
  token_count      INTEGER NOT NULL DEFAULT 0,         -- 精确计数缓存
  usage_json       TEXT,
  created_at       INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_seq ON messages(seq);

-- ============ 运行（预埋多端字段） ============
CREATE TABLE IF NOT EXISTS runs (
  id            TEXT PRIMARY KEY,
  trigger_type  TEXT NOT NULL DEFAULT 'manual',        -- manual | schedule | api
  status        TEXT NOT NULL,                         -- pending|running|waiting_approval|completed|failed|cancelled|interrupted
  stop_reason   TEXT,
  model_ref     TEXT NOT NULL DEFAULT '',
  executor      TEXT NOT NULL DEFAULT 'server',        -- server | desktop | mobile | browser（多端归属）
  device_id     TEXT,                                  -- 端侧执行时记录设备
  parent_run_id TEXT,                                  -- 嵌套 Run（subagent / collaborator）
  input_tokens  INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens INTEGER NOT NULL DEFAULT 0,
  is_estimated  INTEGER NOT NULL DEFAULT 0,
  error_code    TEXT,
  error_message TEXT,
  started_at    INTEGER,
  ended_at      INTEGER,
  created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status);
CREATE INDEX IF NOT EXISTS idx_runs_parent ON runs(parent_run_id);

-- ============ 工具调用事件（审计 + UI 回放；预埋多端与信任级） ============
CREATE TABLE IF NOT EXISTS tool_events (
  id           TEXT PRIMARY KEY,
  run_id       TEXT NOT NULL,
  message_id   TEXT,
  tool_name    TEXT NOT NULL,
  namespace    TEXT NOT NULL DEFAULT 'builtin',        -- builtin | mcp:<server> | plugin:<name> | pkg:<id>
  args_json    TEXT,
  result_json  TEXT,                                   -- >16KB 时只留摘要
  result_blob  TEXT,                                   -- 全文 blob 引用
  status       TEXT NOT NULL,                          -- pending|approved|denied|running|ok|error|timeout
  risk         TEXT NOT NULL DEFAULT 'readonly',       -- readonly|write|command|danger
  executor     TEXT NOT NULL DEFAULT 'server',
  device_id    TEXT,
  source_trust TEXT NOT NULL DEFAULT 'trusted',        -- trusted | untrusted（端侧结果不可信）
  approved_by  TEXT,
  error        TEXT,
  started_at   INTEGER,
  ended_at     INTEGER
);
CREATE INDEX IF NOT EXISTS idx_tool_events_run ON tool_events(run_id);
CREATE INDEX IF NOT EXISTS idx_tool_events_status ON tool_events(status);

-- ============ 审批 ============
CREATE TABLE IF NOT EXISTS approvals (
  id            TEXT PRIMARY KEY,
  run_id        TEXT NOT NULL,
  tool_event_id TEXT NOT NULL,
  request_json  TEXT NOT NULL,
  decision      TEXT,                                  -- allow_once | allow_session | deny
  reason        TEXT,
  requested_at  INTEGER NOT NULL,
  decided_at    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_approvals_run ON approvals(run_id);

-- ============ 会话级审批规则（"本会话允许"；分库后无需 session_id 列） ============
CREATE TABLE IF NOT EXISTS approval_rules (
  id         TEXT PRIMARY KEY,
  tool_name  TEXT NOT NULL,
  pattern    TEXT NOT NULL DEFAULT '',                 -- 命令前缀 / 路径前缀
  created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_approval_rules_uniq ON approval_rules(tool_name, pattern);

-- ============ 会话级 blob 索引（文件在 sessions/<sid>/blobs/，无 ref_count） ============
CREATE TABLE IF NOT EXISTS blobs (
  hash       TEXT PRIMARY KEY,                         -- sha256 hex
  size       INTEGER NOT NULL,
  mime       TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);

-- ============ 会话级设置（tools.tiers 三层覆盖的会话层等） ============
CREATE TABLE IF NOT EXISTS session_settings (
  key        TEXT PRIMARY KEY,
  value_json TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

-- ============ 迁移版本（会话库独立版本线，惰性迁移） ============
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  applied_at INTEGER NOT NULL
);
