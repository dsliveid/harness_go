package retrieval

import "sort"

// DefaultRRFK RRF 平滑常数（业界标准 k=60）。
//
// 为什么用 RRF 而不是分数加权：bm25 与余弦相似度量纲完全不同（前者无界负值、后者 [-1,1]），
// 归一化会引入额外调参且不稳定。RRF 只用名次，天然免疫量纲问题，且无需训练。
const DefaultRRFK = 60

// FusedHit RRF 融合后的命中。
type FusedHit struct {
	DocID       int64
	Key         string
	Score       float64 // RRF 融合分（越大越相关）
	KeywordRank int     // 关键词通道名次（0 = 未命中该通道）
	VectorRank  int     // 向量通道名次（0 = 未命中该通道）
	Cosine      float64 // 原始余弦相似度（供记忆打分复用，避免二次计算）
}

// RRF 用 Reciprocal Rank Fusion 融合两路召回结果。
//
// 公式：score(d) = Σ_channels 1 / (k + rank_channel(d))，rank 从 1 起。
// 只在某一路出现的文档也参与计分（另一路贡献 0），这正是 RRF 能召回「单路独有」结果的原因。
//
// k <= 0 时回落 DefaultRRFK（防调用方传 0 导致除零）。
func RRF(keyword []KeywordHit, vector []VectorHit, k int) []FusedHit {
	if k <= 0 {
		k = DefaultRRFK
	}
	acc := make(map[int64]*FusedHit, len(keyword)+len(vector))
	order := make([]int64, 0, len(keyword)+len(vector))

	get := func(id int64, key string) *FusedHit {
		if h, ok := acc[id]; ok {
			if h.Key == "" && key != "" {
				h.Key = key
			}
			return h
		}
		h := &FusedHit{DocID: id, Key: key}
		acc[id] = h
		order = append(order, id)
		return h
	}

	// 关键词通道：按传入顺序即为名次（实现方已按 bm25 升序返回）。
	for i, hit := range keyword {
		rank := i + 1
		h := get(hit.DocID, hit.Key)
		h.KeywordRank = rank
		h.Score += 1.0 / float64(k+rank)
	}

	// 向量通道：同理，实现方已按余弦降序返回。
	for i, hit := range vector {
		rank := i + 1
		h := get(hit.DocID, hit.Key)
		h.VectorRank = rank
		h.Score += 1.0 / float64(k+rank)
		if hit.Score > h.Cosine {
			h.Cosine = hit.Score
		}
	}

	out := make([]FusedHit, 0, len(order))
	for _, id := range order {
		out = append(out, *acc[id])
	}
	// 稳定排序：分数相同按 DocID 升序，保证结果可复现（测试与缓存前缀都依赖确定性）。
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].DocID < out[j].DocID
	})
	return out
}

// TopN 截断到前 n 条；n <= 0 或超长时原样返回（不 panic）。
func TopN(hits []FusedHit, n int) []FusedHit {
	if n <= 0 || n >= len(hits) {
		return hits
	}
	return hits[:n]
}
