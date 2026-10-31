package memory

// 写入侧四分类去重（阶段 D-3 / mem0 思路，多端协同 §6.3）。
//
// 「同一事实禁止重复落库」——否则历史越长、检索越吵、注入越多。
// 本文件只放**纯函数与阈值**，与存储解耦，保证四分类规则可独立单测。

// Op 写入决策（mem0 四分类）。
type Op string

const (
	// OpAdd 新事实，落库。
	OpAdd Op = "ADD"
	// OpUpdate 同一事实的新表述/语义冲突，覆盖旧条目。
	OpUpdate Op = "UPDATE"
	// OpDelete 与既有事实矛盾，删除旧条目（须由上层编排「先删后写」）。
	OpDelete Op = "DELETE"
	// OpNoop 重复事实，不写（只刷新召回时间，让时间衰减重新起算）。
	OpNoop Op = "NOOP"
)

// 相似度阈值（余弦）。
const (
	// NoopSimilarity 高于此值判「同一事实的重复表述」→ NOOP 不写。
	// 口径出自多端协同 §6.3：相似度 > 0.92 判 NOOP。
	NoopSimilarity = 0.92
	// UpdateSimilarity 高于此值（且不超过 NOOP 阈值）判「同一事实的新表述」→
	// UPDATE 覆盖旧条目。0.80~0.92 是「语义相近但内容不同」的机械近似；
	// 真正的语义冲突/矛盾判定需要语义理解方（LLM/人工）给出 contradicts。
	UpdateSimilarity = 0.80
)

// Similar 既有条目与新条目的相似度（降序喂给 Classify）。
type Similar struct {
	Key   string
	DocID int64
	Score float64 // 余弦相似度 [-1,1]
}

// Classify 四分类纯函数。
//
// 输入契约：
//   - similar：既有条目按相似度**降序**（调用方负责排序，取首位判定）；
//   - contradicts：上层语义判定方（LLM/人工）给出的「与既有事实矛盾」结论。
//     本包不做语义理解，绝不臆测 DELETE —— 没有判定方时恒传 false，
//     此时四分类退化为 ADD/UPDATE/NOOP 三类（机械规则可判定的全部）。
//
// 判定顺序（先判矛盾，再看相似度）：
//
//	contradicts            → DELETE（先删旧条目，写入由调用方随后执行）
//	无候选                 → ADD
//	首位相似度 > 0.92      → NOOP（重复）
//	首位相似度 > 0.80      → UPDATE（覆盖首位对应条目）
//	其余                   → ADD
func Classify(similar []Similar, contradicts bool) Op {
	if contradicts {
		return OpDelete
	}
	if len(similar) == 0 {
		return OpAdd
	}
	switch top := similar[0].Score; {
	case top > NoopSimilarity:
		return OpNoop
	case top > UpdateSimilarity:
		return OpUpdate
	default:
		return OpAdd
	}
}
