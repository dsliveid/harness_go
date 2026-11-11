-- 主库 0011：设备记录归属账号（设备与账号持久绑定）
-- 铁律：已发布的迁移文件永不修改，只能追加新文件（0002 / 0010 保持原样）。

-- 为什么必须记 user_id：
--   0010 让**配对码**携带授权者，解决了「配对那一下不用再登录」——但配对码是一次性的，
--   兑换后即消费。于是「这台设备属于哪个账号」在配对完成后就再也没有落点了，
--   产生两个真实故障：
--
--   ① 7 天失效：user_token 随 session_ttl_h（默认 168h）过期后，端侧只剩 device_token，
--      而 device_token 通不过 /api/** 的用户令牌中间件（VerifyToken 只查 api_tokens 表）。
--      没有归属关系，服务端就无法回答「该给谁续一张新会话」。
--   ② 归属不可见：设备列表 / SSE 状态推送都无法展示「这台机器属于谁」，
--      多用户场景下没有任何按账号过滤的基础。
--
--   把归属落到 devices 上，配合 POST /api/v1/devices/session（凭设备令牌换新会话），
--   才能让「绑定即免登录」从「一次性授权」变成「持久关系」。
--
-- 兼容性：
--   ALTER TABLE ... ADD COLUMN ... NOT NULL DEFAULT '' 在 SQLite 上合法且对既有行无影响。
--   历史设备 user_id 为空串 = 归属未知 —— 这是**合法状态**，不参与鉴权，
--   只表示「不能为它签发用户会话」，故 /devices/session 对它返回 device.owner_unknown。
--   这类设备需重新配对才能获得归属，绝不猜测、绝不回填成某个账号。
ALTER TABLE devices ADD COLUMN user_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_devices_user ON devices(user_id);
