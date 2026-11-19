-- 0015 会话已读游标：支撑侧栏「未读红点」。
--
-- 背景（2026-11）：前端 types.ts 早有 `unread?: boolean`、Sidebar.tsx 也早已渲染红点，
-- 但服务端**没有任何对应数据** → 判定恒为 undefined，红点永不出现。
-- 本迁移补上唯一缺失的那一列。
--
-- 🚨 为什么不存「未读布尔值」
--   unread 是**派生值**：unread = last_message_at > last_read_at。
--   存布尔必然与真实消息时间漂移（改一条消息时间就要同步改布尔，迟早不一致），
--   且无法表达「已读到哪一刻」。只存游标，判定永远由两列比较得出，天然自洽。
--
-- 🚨 为什么回填为 last_message_at 而不是 0
--   DEFAULT 0 会让**所有存量会话**在升级瞬间变成未读 → 用户一打开就看到满屏红点，
--   这是纯粹的惊扰（那些消息他早就读过了）。保守起见统一回填为「已读」：
--   红点只对升级**之后**新产生的消息生效。
--
-- 铁律（迁移规范，见 0008 文件头）：
--   1. 已发布的迁移文件**永不修改**，任何变更只能追加新的 NNNN_xxx.sql；
--   2. 本文件由 applyMigrations 在**独立事务**内执行，SQL 内严禁出现 BEGIN/COMMIT；
--   3. ALTER TABLE ... ADD COLUMN 不可重复执行，仅靠版本号保证只跑一次。

ALTER TABLE sessions ADD COLUMN last_read_at INTEGER NOT NULL DEFAULT 0;

-- 存量会话视为「已读」：把游标推到当前最后消息时间（无消息的会话保持 0）。
-- COALESCE 兜住 last_message_at 为 NULL 的空会话（从未发过消息，不该显示红点）。
UPDATE sessions SET last_read_at = COALESCE(last_message_at, 0);
