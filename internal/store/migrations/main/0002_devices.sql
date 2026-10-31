-- 主库 0002：设备与 Runner（阶段 C / 多端协同 HALO M5）
-- 依据 docs/数据存储方案.md v2.0 §2.3：devices / capabilities / session_leases 阶段 C 加入主库。
-- 全部在主库（跨端可见），会话库不动业务表 → 满足"零跨库事务"铁律。
-- 铁律：已发布的迁移文件永不修改，只能追加新文件。

-- ============ 已配对设备（令牌只存 sha256，明文永不入库） ============
CREATE TABLE IF NOT EXISTS devices (
  id                TEXT PRIMARY KEY,
  name              TEXT NOT NULL DEFAULT '',
  kind              TEXT NOT NULL DEFAULT 'desktop',   -- desktop | tablet | mobile | cli | browser
  platform          TEXT NOT NULL DEFAULT '',          -- windows | darwin | linux | android | ios
  status            TEXT NOT NULL DEFAULT 'offline',   -- online | offline | revoked
  capabilities_hash TEXT NOT NULL DEFAULT '',          -- 能力集合摘要，变化即触发重新下发
  protocol_version  INTEGER NOT NULL DEFAULT 1,        -- 协议版本（服务端支持 N / N-1）
  app_version       TEXT NOT NULL DEFAULT '',
  load_json         TEXT,                              -- 最近一次 ping 上报的负载快照
  token_hash        TEXT NOT NULL,                     -- sha256(device_token)
  paired_at         INTEGER NOT NULL,
  last_seen_at      INTEGER,
  revoked_at        INTEGER,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_devices_token ON devices(token_hash);
CREATE INDEX IF NOT EXISTS idx_devices_status ON devices(status, last_seen_at DESC);
CREATE INDEX IF NOT EXISTS idx_devices_kind ON devices(kind);

-- ============ 一次性配对码（TTL 120s，落库以便服务重启不打断配对窗口） ============
CREATE TABLE IF NOT EXISTS device_pair_codes (
  code        TEXT PRIMARY KEY,                        -- 6 位数字
  device_name TEXT NOT NULL DEFAULT '',                -- 端侧预留的名称（可空）
  device_kind TEXT NOT NULL DEFAULT '',
  expires_at  INTEGER NOT NULL,
  consumed_at INTEGER,                                 -- 非 NULL 即已消费（一次性）
  created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_device_pair_codes_expires ON device_pair_codes(expires_at);

-- ============ 设备能力（按设备维度全量替换） ============
CREATE TABLE IF NOT EXISTS capabilities (
  device_id  TEXT NOT NULL,
  name       TEXT NOT NULL,
  version    TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (device_id, name)
);

-- ============ 会话租约（跨端单写者；device_id 为空串表示服务端本地持有） ============
CREATE TABLE IF NOT EXISTS session_leases (
  session_id   TEXT PRIMARY KEY,
  device_id    TEXT NOT NULL DEFAULT '',
  epoch        INTEGER NOT NULL DEFAULT 1,             -- 每次接管 +1，旧端收 lease.revoked
  expires_at   INTEGER NOT NULL,
  heartbeat_at INTEGER NOT NULL,
  created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_session_leases_expires ON session_leases(expires_at);
