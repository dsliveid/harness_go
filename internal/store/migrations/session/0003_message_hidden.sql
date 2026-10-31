-- 阶段 G：消息置灰（S10 消息级回滚）
-- 回滚点之后的消息**置灰但保留**（audit 可见），引擎组装下一轮上下文时排除。

ALTER TABLE messages ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0;
