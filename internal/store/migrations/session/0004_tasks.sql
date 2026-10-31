-- 阶段 G：长任务（Host.Tasks）+ 回滚事件（S10 revert & reapply 审计）

-- 长任务状态机持久态（ADR-009：进程重启后在途任务由启动恢复流程收敛，
-- 这里只保证已登记的任务行与检查点可查询）。
CREATE TABLE IF NOT EXISTS tasks (
  id         TEXT PRIMARY KEY,
  run_id     TEXT NOT NULL DEFAULT '',
  title      TEXT NOT NULL,
  state      TEXT NOT NULL,                    -- pending|running|paused|completed|failed|cancelled
  steps_json TEXT NOT NULL DEFAULT '[]',
  meta_json  TEXT,                             -- 含 checkpoint 用 paths 声明
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tasks_state ON tasks(state);

-- 检查点 = Run + 快照引用（快照行在主库，跨库无事务：
-- 先落快照（文件+主库行），再落本表；本表失败时补偿删除快照行）。
CREATE TABLE IF NOT EXISTS task_checkpoints (
  id          TEXT PRIMARY KEY,
  task_id     TEXT NOT NULL,
  run_id      TEXT NOT NULL DEFAULT '',
  snapshot_id TEXT NOT NULL DEFAULT '',
  note        TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_task_checkpoints_task ON task_checkpoints(task_id);

-- 回滚事件：一次 revert 的完整审计 + reapply 的依据（恢复哪些 after 快照、
-- 置灰了哪些消息）。reapply 后 reapplied_at 置时间戳，不可重复 reapply。
CREATE TABLE IF NOT EXISTS rollbacks (
  id               TEXT PRIMARY KEY,
  mode             TEXT NOT NULL,              -- tool | turn | message
  target           TEXT NOT NULL,              -- tool_event_id | run_id | message seq（文本）
  affected_json    TEXT NOT NULL DEFAULT '[]', -- 参与回滚的快照锚点 [{event_id,run_id,snapshot_before,snapshot_after,paths}]
  hidden_from_seq  INTEGER NOT NULL DEFAULT 0, -- 消息级回滚置灰起点（0 = 未置灰消息）
  restored_json    TEXT NOT NULL DEFAULT '[]', -- 物理写回结果 [{path,action,bytes}]
  created_at       INTEGER NOT NULL,
  reapplied_at     INTEGER
);
