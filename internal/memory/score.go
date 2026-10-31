package memory

import "math"

// 检索打分（阶段 D-3 / 多端协同 §6.3）。
//
//	score = 0.6*cos + 0.25*时间衰减(半衰期 14d) + 0.15*重要性
//
// 命中不足 MinRecallHits 条则整层不注入（宁缺毋滥）——
// 三条低分记忆只会稀释上下文，不会帮到模型。

const (
	// WeightCos 余弦相似度权重（相关性主信号）。
	WeightCos = 0.6
	// WeightDecay 时间衰减权重。
	WeightDecay = 0.25
	// WeightImportance 重要性权重（0~1，写入时给定）。
	WeightImportance = 0.15
	// HalfLifeDays 时间衰减半衰期（天）：距上次召回 14 天，衰减分减半。
	HalfLifeDays = 14
	// MinRecallHits 整层注入的最少命中数（不足则该层不注入）。
	MinRecallHits = 3
	// RecallTopK 每层最多注入条数（多端协同 §6.1 量级估算取 top6×80）。
	RecallTopK = 6
)

// Decay 时间衰减因子 = 0.5^(age_days / 14)。
//
// lastRefAtMs > nowMs（时钟回拨/脏数据）按 0 天处理（衰减分 1.0），
// 不返回 >1 的值污染总分排序。
func Decay(lastRefAtMs, nowMs int64) float64 {
	const msPerDay = 24 * 60 * 60 * 1000
	ageDays := float64(nowMs-lastRefAtMs) / msPerDay
	if ageDays < 0 {
		ageDays = 0
	}
	return math.Pow(0.5, ageDays/HalfLifeDays)
}

// Score 记忆打分纯函数。
//
// cos 取值 [-1,1]：无向量通道（未配置 embedding）时传 0 ——
// 这是**诚实降级**：排序退化为「时间衰减 + 重要性」两个可算的信号，
// 而不是臆造一个相似度。importance 取值 [0,1]，越界值在此钳制。
func Score(cos float64, lastRefAtMs, nowMs int64, importance float64) float64 {
	if cos < -1 {
		cos = -1
	} else if cos > 1 {
		cos = 1
	}
	if importance < 0 {
		importance = 0
	} else if importance > 1 {
		importance = 1
	}
	return WeightCos*cos + WeightDecay*Decay(lastRefAtMs, nowMs) + WeightImportance*importance
}
