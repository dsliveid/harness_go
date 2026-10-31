-- 阶段 G：全局文件快照（S10 快照与回滚服务）
-- docs/数据存储方案.md §3：快照走全局 CAS（data/snapshots/blobs/<sha256>），
-- 同一文件版本跨会话只存一份，元数据（快照行 + blob 引用计数）落主库。

-- 快照行：一次 Create = 一组路径在某时刻的内容引用（hash="" 表示当时不存在）。
-- 锚点列（session/run/tool_event/task）由插件 ctx 中的调用信息自动填充，
-- 回滚服务据此按 消息/轮次/工具级 定位要恢复的快照。
CREATE TABLE IF NOT EXISTS snapshots (
  id            TEXT PRIMARY KEY,
  session_id    TEXT NOT NULL DEFAULT '',
  run_id        TEXT NOT NULL DEFAULT '',
  tool_event_id TEXT NOT NULL DEFAULT '',
  task_id       TEXT NOT NULL DEFAULT '',      -- 长任务检查点来源（kind=checkpoint）
  kind          TEXT NOT NULL DEFAULT 'before', -- before | after | checkpoint
  paths_json    TEXT NOT NULL,                  -- [{path,hash,size,absent}]
  size          INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_snapshots_session ON snapshots(session_id, created_at);
CREATE INDEX IF NOT EXISTS idx_snapshots_event   ON snapshots(tool_event_id);

-- 全局 blob 引用计数：快照创建 +1，快照行删除（会话删除/补偿回退）-1，
-- ref_count 归零的 blob 由 GC 物理清理。
CREATE TABLE IF NOT EXISTS snapshot_blobs (
  hash      TEXT PRIMARY KEY,
  size      INTEGER NOT NULL,
  ref_count INTEGER NOT NULL DEFAULT 0
);
