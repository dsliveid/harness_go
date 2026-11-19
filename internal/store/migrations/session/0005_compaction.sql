-- 上下文压缩（compaction）：长会话的摘要持久化。
--
-- # 为什么需要这张表
--
-- 工具输出入库定型（resultfmt）解决的是「单条输出过大」；本表解决的是
-- 「历史轮次过多」。二者缺一不可：定型后单条变小了，但 100 轮累积起来
-- 仍会超出上下文窗口。
--
-- 真实事故（会话 9ee34e00）：单 Run 内 input token 累积到 1,090,444，
-- 而 output 仅 29,144（37 倍）。当时的降级链第五级「强制压缩」只有信号、
-- 没有消费者（internal/prompt/budget.go:31），实际从未执行过。
--
-- # 语义
--
-- 一行代表一次压缩：把 [start_seq, end_seq] 区间的消息总结成 summary_markdown。
-- 读取历史时，凡 seq <= 任一压缩行 end_seq 的消息，一律替换为该行的摘要，
-- 不再逐条注入。
--
-- 关键不变量：
--   - end_seq 单调递增（同一会话的压缩区间不重叠，否则回放顺序会错乱）；
--   - 只压缩**已完成轮次**，绝不切断当前活跃轮次（见 compact.Service 的分区逻辑）；
--   - 摘要一旦写入不可修改（与 messages 的不可变语义一致）。
CREATE TABLE IF NOT EXISTS session_compactions (
  id               TEXT PRIMARY KEY,
  -- 被压缩区间的起止 seq（含端点）。start_seq 仅用于排障与回溯。
  start_seq        INTEGER NOT NULL,
  end_seq          INTEGER NOT NULL,
  -- 摘要正文（markdown）。替代区间内全部消息进入后续上下文。
  summary_markdown TEXT NOT NULL,
  -- 被替代的消息条数与原始估算 token（用于统计压缩收益，不做业务判断）。
  replaced_messages INTEGER NOT NULL DEFAULT 0,
  replaced_tokens   INTEGER NOT NULL DEFAULT 0,
  -- 压缩发生时的模型引用（排障用：不同模型的摘要质量差异很大）。
  model_ref        TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL
);

-- 读取路径固定为「按 end_seq 倒序取最大」，这个索引直接服务该查询。
CREATE INDEX IF NOT EXISTS idx_compactions_end_seq ON session_compactions(end_seq);
