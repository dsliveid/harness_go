// Package retrieval 实现 Token 治理的检索内核（阶段 D / M6）。
//
// 定位：本包零 store 依赖，只定义检索算法与数据形态，仓储实现由 internal/store 提供。
// 这样 store 实现「关键词 + 向量」两条通道，本包负责融合（RRF）与打分，互不越界。
//
// 设计依据（三条实测结论）：
//   - FTS5 可用（modernc.org/sqlite，SQLite 3.54.1 纯 Go）→ 关键词通道走 bm25()；
//   - sqlite-vec 需 CGO，违反「纯 Go 无 CGO 单二进制」铁律 → 向量走 float32 BLOB + 暴力余弦；
//   - 索引表必须带 model_id + dim，换 embedding 模型即重建。
package retrieval

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Kind 索引文档类别（与 retrieval_docs.kind 严格一致）。
type Kind string

const (
	// KindTool 工具索引（deferred 档按需注入的候选池）。
	KindTool Kind = "tool"
	// KindMemoryL2 记忆情景层（digest 碎记，向量检索主战场）。
	KindMemoryL2 Kind = "memory_l2"
	// KindMemoryL3 记忆档案层（全量文档，检索后按 path:line 引用）。
	KindMemoryL3 Kind = "memory_l3"
)

// Valid 判断类别是否为已知值。
func (k Kind) Valid() bool {
	switch k {
	case KindTool, KindMemoryL2, KindMemoryL3:
		return true
	default:
		return false
	}
}

// ErrDimMismatch 向量维度不一致（换模型未重建索引时的显式错误，禁止静默跳过）。
var ErrDimMismatch = errors.New("retrieval: 向量维度不一致，需重建索引")

// ErrEmptyVector 空向量。
var ErrEmptyVector = errors.New("retrieval: 空向量")

// EncodeVector 把 float32 切片编码为小端连续 BLOB（SQLite BLOB 列直接存储）。
//
// 编码格式：dim 个 float32，每个 4 字节小端，总长 = 4*dim。
// 选择小端的原因：跨平台可移植（x86/arm 均小端，大端机也按显式字节序读写）。
func EncodeVector(v []float32) ([]byte, error) {
	if len(v) == 0 {
		return nil, ErrEmptyVector
	}
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf, nil
}

// DecodeVector 解码 BLOB 为 float32 切片；长度非 4 的倍数即报错。
func DecodeVector(b []byte) ([]float32, error) {
	if len(b) == 0 {
		return nil, ErrEmptyVector
	}
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("retrieval: 向量字节长度 %d 不是 4 的倍数", len(b))
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v, nil
}

// Dim 返回 BLOB 对应的向量维度。
func Dim(b []byte) int { return len(b) / 4 }

// Cosine 计算余弦相似度，取值 [-1, 1]。
//
// 边界：任一向量为零向量或维度不一致时返回 0（不 panic，也不返回 NaN ——
// NaN 会污染排序比较，导致 sort.Slice 结果不确定）。
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// VectorHit 向量通道命中。
type VectorHit struct {
	DocID   int64
	Key     string
	Score   float64 // 余弦相似度 [-1,1]
	Dim     int
	ModelID string // 命中所属 embedding 模型（用于换模型后判定过期）
}

// VectorSearcher 向量通道端口（由 store 实现：读出候选集后在内存中暴力余弦）。
type VectorSearcher interface {
	// SearchVector 返回 topN 命中，按 Score 降序；modelID 为空表示不限模型。
	// 实现方必须在候选集为空或模型不匹配时返回空切片而非错误（召回为空是正常态）。
	SearchVector(query []float32, modelID string, topN int) ([]VectorHit, error)
}

// KeywordHit 关键词通道命中。
type KeywordHit struct {
	DocID int64
	Key   string
	BM25  float64 // FTS5 bm25()，**越小越相关**（SQLite 返回负值，绝对值越大越相关）
	Rank  int     // 通道内名次（1 起）
}

// KeywordSearcher 关键词通道端口（由 store 实现：FTS5 MATCH + bm25 排序）。
type KeywordSearcher interface {
	// SearchKeyword 返回 topN 命中，按 bm25 升序（最相关在前）；无命中返回空切片。
	SearchKeyword(query string, topN int) ([]KeywordHit, error)
}
