-- 0009 工作空间域：把「工作空间」（执行面 / 客户端接入点）落库为服务端一等实体。
--
-- 🚨 铁律（迁移规范）：
--   1. 已发布的迁移文件**永不修改**，任何变更只能追加新的 NNNN_xxx.sql；
--   2. 本文件由 applyMigrations 在**独立事务**内执行，SQL 内严禁出现 BEGIN/COMMIT；
--   3. ALTER TABLE ... ADD COLUMN 不可重复执行，仅靠版本号保证只跑一次。
--
-- 🚨 顺序铁律（docs/工作空间模型与三端能力收口协议.md §9.1.3，顺序不可颠倒）：
--   ① 建表 → ② 建索引 → ③ 插内置空间 ws-server → ④ 加列 → ⑤ 回填既有行。
--   若先回填后插 ws-server，会把历史上的会话/项目指向一个不存在的空间（悬空外键）。
--   建表与插入必须幂等（IF NOT EXISTS / OR IGNORE），保证中途失败可重跑。

-- ① 工作空间表。
-- kind：能力天花板依据（server | desktop | mobile | browser），**只由本表裁决**，与会话创建方无关。
-- device_id：空串 = 服务端内置空间（无端侧宿主）；非空 = 归属某台已配对设备。
-- root_path：该空间在宿主侧的根目录（服务端内置空间为空串，由会话自身 workspace_path 决定）。
CREATE TABLE IF NOT EXISTS workspaces (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  device_id TEXT NOT NULL DEFAULT '',
  root_path TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

-- ② 索引：按设备反查空间（设备重连/配对自动落空间）、按 kind 过滤（能力天花板筛选）。
CREATE INDEX IF NOT EXISTS idx_workspaces_device ON workspaces(device_id);
CREATE INDEX IF NOT EXISTS idx_workspaces_kind ON workspaces(kind);

-- ③ 内置服务端空间（必须早于回填）。时间戳用 SQLite 表达式，避免文件内硬编码动态值。
INSERT OR IGNORE INTO workspaces(id, name, kind, device_id, root_path, created_at, updated_at)
VALUES ('ws-server', '服务端', 'server', '', '',
        CAST(strftime('%s', 'now') AS INTEGER) * 1000,
        CAST(strftime('%s', 'now') AS INTEGER) * 1000);

-- ④ 加列：SQLite ALTER TABLE ADD COLUMN 只允许可空列（回填在下一步）。
--    理由与 sessions.project_id 不同：这里允许 NULL，是为了让"未回填"可被识别
--    （0008 的 project_id 用 NOT NULL DEFAULT ''，语义是"空串=纯对话"；workspace 不允许空）。
ALTER TABLE projects ADD COLUMN workspace_id TEXT;
ALTER TABLE sessions ADD COLUMN workspace_id TEXT;

-- ⑤ 回填：历史项目/会话一律归入服务端内置空间（§11.1 已定案，无倒退）。
UPDATE projects SET workspace_id = 'ws-server' WHERE workspace_id IS NULL;
UPDATE sessions SET workspace_id = 'ws-server' WHERE workspace_id IS NULL;

-- 回填后补索引（Delete 前要先查引用，列表/统计也可能按空间过滤）。
CREATE INDEX IF NOT EXISTS idx_sessions_workspace ON sessions(workspace_id);
CREATE INDEX IF NOT EXISTS idx_projects_workspace ON projects(workspace_id);
