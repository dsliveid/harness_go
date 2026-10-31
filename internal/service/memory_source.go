package service

import "context"

// MemorySource 记忆召回端口（阶段 D-3）。
//
// 定位：RunService 每 Run 把「本轮用户输入」交给实现方，拿回两个注入块：
//   - core：L0 核心常驻（≤800 token）+ L1 项目三行摘要 —— 进 system 稳定前缀；
//   - tail：L2/L3 检索命中 —— 置尾动态块，也是预算降级第①级的省略对象。
//
// 铁律（与工具检索同口径）：召回失败**不阻断对话** —— 记忆是优化项，
// 由调用方降级为空块并继续；绝不让索引/向量故障变成所有 Run 不可用。
type MemorySource interface {
	RecallBlocks(ctx context.Context, query string) (core, tail string, err error)
}
