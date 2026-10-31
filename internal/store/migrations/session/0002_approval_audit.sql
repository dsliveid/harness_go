-- 会话库 0002：审批跨端仲裁审计列（阶段 C / 多端协同 §5.4）
-- 仅追加审计列，向后兼容（SQLite ALTER TABLE ADD COLUMN 对既有行填 NULL）。
-- 严格模式（devices.strict_approval_device=true）下由 service 层判定，
-- 本库只记录"哪个设备发起 / 哪个设备决策"，不做约束。
-- 铁律：已发布的迁移文件永不修改，只能追加新文件。

ALTER TABLE approvals ADD COLUMN requested_by_device TEXT;
ALTER TABLE approvals ADD COLUMN decided_by_device TEXT;
