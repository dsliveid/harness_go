-- 主库初始化（docs/数据存储方案.md v2.0 §2.2）
-- 主库存放"跨会话共享"的数据；会话域数据在 sessions/<sid>/session.db。
-- 铁律：已发布的迁移文件永不修改，只能追加新文件。

-- ============ 会话（含摘要冗余列，列表/统计只读本表） ============
CREATE TABLE IF NOT EXISTS sessions (
  id               TEXT PRIMARY KEY,
  title            TEXT NOT NULL DEFAULT '',
  workspace_path   TEXT NOT NULL DEFAULT '',
  access_mode      TEXT NOT NULL DEFAULT 'confirm',   -- confirm | full
  model_ref        TEXT NOT NULL DEFAULT '',
  user_id          TEXT NOT NULL DEFAULT '',

  -- 摘要冗余列：写消息 / Run 状态迁移后异步回写（最终一致）
  message_count    INTEGER NOT NULL DEFAULT 0,
  last_message_at  INTEGER,
  total_tokens     INTEGER NOT NULL DEFAULT 0,
  last_run_status  TEXT,

  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL,
  deleted_at       INTEGER
);
CREATE INDEX IF NOT EXISTS idx_sessions_updated ON sessions(deleted_at, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_user    ON sessions(user_id, updated_at DESC);

-- ============ 本地账号（argon2id） ============
CREATE TABLE IF NOT EXISTS users (
  id            TEXT PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,                        -- argon2id 编码串
  display_name  TEXT NOT NULL DEFAULT '',
  role          TEXT NOT NULL DEFAULT 'owner',
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

-- ============ 令牌（API / 网页会话 / SSO 统一） ============
CREATE TABLE IF NOT EXISTS api_tokens (
  id           TEXT PRIMARY KEY,
  user_id      TEXT NOT NULL DEFAULT '',
  name         TEXT NOT NULL DEFAULT '',
  kind         TEXT NOT NULL DEFAULT 'api',           -- api | web_session | sso
  token_hash   TEXT NOT NULL,                         -- sha256(token)，库内不存明文
  scope        TEXT NOT NULL DEFAULT 'write',         -- read | write | admin
  expires_at   INTEGER,
  last_used_at INTEGER,
  revoked_at   INTEGER,
  created_at   INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_tokens_hash ON api_tokens(token_hash);
CREATE INDEX IF NOT EXISTS idx_api_tokens_user ON api_tokens(user_id, kind);

-- ============ 模型厂商 ============
CREATE TABLE IF NOT EXISTS providers (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  base_url    TEXT NOT NULL DEFAULT '',
  api_key_enc BLOB,                                   -- AES-256-GCM
  models_json TEXT NOT NULL DEFAULT '[]',
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);

-- ============ 键值配置（含 tools.tiers 三层覆盖的全局层） ============
CREATE TABLE IF NOT EXISTS settings (
  key        TEXT PRIMARY KEY,
  value_json TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

-- ============ 迁移版本（主库独立版本线） ============
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  applied_at INTEGER NOT NULL
);
