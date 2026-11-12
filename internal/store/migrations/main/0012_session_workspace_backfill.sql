-- 主库 0012：存量会话回填工作空间归属（R1 工作空间必填）
-- 铁律：已发布的迁移文件永不修改，只能追加新文件。

-- 背景（为什么需要回填）：
--   R1 要求「任何会话都必须绑定工作空间，纯对话也不例外」。应用层已改为
--   缺省绑定 ws-server（session_service.Create），但**存量行**仍是空串：
--   它们会走到 resolveToolPolicy 的 wsID == "" 异常分支，且 OpenAI 兼容端点 /
--   bundle 导入等历史会话都无法正常发起对话。
--
-- 回填方向：workspace_id = '' 或 NULL  →  store.BuiltinWorkspaceServerID（"ws-server"）
--
-- 为什么是 ws-server 而不是别的：
--   1. 它是恒在线的服务端内置执行面，不需要任何设备配合，回填后会话立刻可用；
--   2. 这些会话**不设 project_id**，故 resolveToolPolicy 判为 readonly（纯对话口径，R1b）
--      —— 拿不到命令执行/文件写入能力；
--   3. 这一点是安全前提：级联删除的既有铁律要求「会话绝不能被迁到 ws-server 防提权」
--      （见 workspace_delete_integration_test.go:TestWorkspaceDeleteNoPrivilegeEscalation）。
--      本迁移只回填**本就无归属**的历史会话，不迁任何已归属设备空间的会话，
--      且回填后因无项目而保持 readonly，不构成提权。
--
-- 为什么不在 DB 层加 NOT NULL 约束：
--   应用层已保证必填，加约束会让迁移顺序与历史备份包恢复产生额外耦合；
--   保留宽松约束 + 应用层兜底，回滚与数据修复都更容易。
--
-- 幂等性：只命中空值行，重复执行无副作用。
UPDATE sessions
SET workspace_id = 'ws-server'
WHERE workspace_id IS NULL OR TRIM(workspace_id) = '';

-- 语义保证：回填后的会话若无项目，能力天花板必须是 readonly。
-- 这里不做数据写入（能力由 resolveToolPolicy 运行时裁决），仅作为口径记录。
