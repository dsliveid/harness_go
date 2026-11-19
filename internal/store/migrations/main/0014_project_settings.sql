-- 0014 项目设置全量持久化：约束 / 规划模式 / 关联项目 / 数据权限。
--
-- 背景（2026-11 实测）：此前「项目设置」6 块配置的持久化状态严重不一致——
--   项目约束(constraints)、规划模式(planMode)、关联项目(projectLinks) 只在前端
--   localStorage；数据权限(dataGrants) 更是**仅内存态，刷新即丢**
--   （chatStore.ts 里那句「服务端未持久化，刷新页面后失效」的注释即为自认）。
-- 用户诉求「项目设置需要长期保存」，故本次把剩余 4 块全部落主库，
-- 与既有的 default_access_mode / project_approval_rules 同源。
--
-- 🚨 铁律（迁移规范，见 0008 文件头）：
--   1. 已发布的迁移文件**永不修改**，任何变更只能追加新的 NNNN_xxx.sql；
--   2. 本文件由 applyMigrations 在**独立事务**内执行，SQL 内严禁出现 BEGIN/COMMIT；
--   3. ALTER TABLE ... ADD COLUMN 不可重复执行，仅靠版本号保证只跑一次。

-- ① 项目约束正文 + 规划与执行策略（项目自身标量属性，放主表免 JOIN）。
--    两者都带 DEFAULT，对存量行安全（老项目的约束为空、规划模式回落 standard）。
ALTER TABLE projects ADD COLUMN constraints TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN plan_mode TEXT NOT NULL DEFAULT 'standard';

-- ② 关联项目：项目设置中对**另一目录**的引用 + 说明（只存路径，不存对方 id）。
--    与 project_approval_rules 同构，都是「项目级子集合」表。
CREATE TABLE IF NOT EXISTS project_links (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  path TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_project_links_pid ON project_links(project_id);

-- ③ 数据权限策略：工作区外路径 / 会话导出 / 数据库查询 / 外部外发 × 拒绝|询问|允许。
--
-- 🚨 不预置任何行：无记录即视为「询问」(ask)，与前端既有 `?? 'ask'` 兜底语义一致。
--    预置行会让「用户从未表态」与「用户明确选了 ask」不可区分，后续想改默认值也会被脏数据卡住。
CREATE TABLE IF NOT EXISTS project_data_grants (
  project_id TEXT NOT NULL,
  scope      TEXT NOT NULL,   -- fs_outside | session_export | db_query | net_egress
  mode       TEXT NOT NULL,   -- deny | ask | allow
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_project_grants_uniq ON project_data_grants(project_id, scope);
