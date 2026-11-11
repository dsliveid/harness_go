-- 主库 0010：配对码记录授权用户（设备配对后免二次登录）
-- 铁律：已发布的迁移文件永不修改，只能追加新文件（0002 保持原样）。

-- 为什么必须记 user_id：
--   POST /api/v1/devices/pair-codes 需用户令牌（不在 publicPaths），即服务端在签发
--   配对码的那一刻**已经知道是哪个账号在授权这台设备**。此前该身份被直接丢弃，
--   导致端侧配对成功后只能证明「这台机器被允许接入」，无法证明「操作者是谁」，
--   于是必须再登录一次。把授权者落到配对码上，配对兑换时即可一并签发用户会话，
--   端侧一次配对直达已登录态。
--
-- 兼容性：ADD COLUMN + NOT NULL DEFAULT '' 在 SQLite 上合法且对既有行无影响；
--   历史配对码 user_id 为空串 → 兑换时视为「无授权者」，正常配对但不下发用户会话
--   （端侧回落登录页）。这是**合法降级路径**，不是错误分支。
ALTER TABLE device_pair_codes ADD COLUMN user_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_device_pair_codes_user ON device_pair_codes(user_id);
