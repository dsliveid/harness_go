-- 0008 项目域：把「项目」落库为服务端一等实体（工作区 + 默认控制权限 + 项目审批规则）。
--
-- 🚨 铁律（迁移规范）：
--   1. 已发布的迁移文件**永不修改**，任何变更只能追加新的 NNNN_xxx.sql；
--   2. 本文件由 applyMigrations 在**独立事务**内执行，SQL 内严禁出现 BEGIN/COMMIT；
--   3. ALTER TABLE ... ADD COLUMN 不可重复执行，仅靠版本号保证只跑一次。

-- 项目实体。
-- default_access_mode：项目级「默认控制权限」快照源，新建会话时一次性继承（confirm | full）。
-- pinned：置顶（列表按 pinned DESC, updated_at DESC 排序）。
-- deleted_at：软删；软删后列表/Get 均不可见。
CREATE TABLE IF NOT EXISTS projects (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  path TEXT NOT NULL DEFAULT '',
  default_access_mode TEXT NOT NULL DEFAULT 'confirm',
  pinned INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_projects_updated ON projects(deleted_at, updated_at DESC);

-- 项目审批规则（阶段四的下发源）：命中 (tool_name, pattern) 即直接放行、不进审批闸门。
CREATE TABLE IF NOT EXISTS project_approval_rules (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  tool_name TEXT NOT NULL DEFAULT '',
  pattern TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_project_rules_uniq ON project_approval_rules(project_id, tool_name, pattern);
CREATE INDEX IF NOT EXISTS idx_project_rules_pid ON project_approval_rules(project_id);

-- 会话归属项目：空串 = 纯对话（不隶属任何项目），保持既有行为完全不变。
ALTER TABLE sessions ADD COLUMN project_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project_id, updated_at DESC);
