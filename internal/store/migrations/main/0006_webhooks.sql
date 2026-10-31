-- P1 接入增强：Webhook 事件订阅（需求 §4.4）+ 会话来源标记（OpenAI 兼容端点的隐藏会话）

-- Webhook 订阅：url + secret（AES-256-GCM 密文，与 providers.api_key 同一加密器）
-- + 事件白名单（空数组 = 全部一期事件）。失败重试与超时在分发器内。
CREATE TABLE IF NOT EXISTS webhooks (
  id          TEXT PRIMARY KEY,
  url         TEXT NOT NULL,
  secret_enc  BLOB,
  events_json TEXT NOT NULL DEFAULT '[]',
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  INTEGER NOT NULL
);

-- 会话来源：ui（默认）/ openai（OpenAI 兼容端点自动创建的隐藏会话）。
-- 隐藏会话不出现在默认列表（source='ui' 过滤），但按 ID 仍可访问（X-Session-ID 复用）。
ALTER TABLE sessions ADD COLUMN source TEXT NOT NULL DEFAULT 'ui';
