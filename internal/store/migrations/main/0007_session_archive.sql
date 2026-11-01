-- 会话归档（对齐桌面端「归档中心」语义）：
-- 归档 = archived_at 置时间戳；取消归档 = 置 NULL。
-- 归档会话仍在主库列表返回（archived_at 随行），由前端分组：侧栏只显示未归档，
-- 归档中心（设置 → 高级 → 归档会话）单独呈现；删除语义不变（deleted_at）。
ALTER TABLE sessions ADD COLUMN archived_at INTEGER;
