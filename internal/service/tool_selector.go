package service

import (
	"context"
	"strings"

	"harness-go/internal/prompt"
	"harness-go/internal/retrieval"
)

// ToolSelector 动态工具检索端口（Token 治理 D-2）。
//
// 定位：RunService 每轮把「用户输入 + 当前候选工具」交给实现方，
// 实现方返回本轮应「提升为全量注入」的工具名。返回空切片是合法结果
// （上层会走「核心集 + 最近使用」回退，见 retrieval.SelectTools）。
//
// 铁律（宽进严出）：返回结果只决定注入深度，**不决定能否执行** ——
// 未被选中的 deferred 工具仍然可以被模型调用（若它猜对了名字与参数）。
type ToolSelector interface {
	// SelectForPrompt 返回本轮需注入全量定义的工具名（保序，最多 topK 条）。
	SelectForPrompt(ctx context.Context, userInput string, candidates []prompt.ToolRef) []string
}

// ToolSearchRepo 检索仓储窄接口（只声明本文件需要的两个方法）。
//
// 为什么在 service 侧定义而不是直接用 *store.RetrievalRepo：
// 依赖倒置 —— 使用方定义窄接口，store 实现即可，service 不依赖 store 具体类型，
// 也便于测试注入假实现（与 agent/ports.go 的做法一致）。
type ToolSearchRepo interface {
	SearchKeyword(ctx context.Context, kind retrieval.Kind, query string, topN int) ([]retrieval.KeywordHit, error)
	SearchVector(ctx context.Context, kind retrieval.Kind, query []float32, modelID string, topN int) ([]retrieval.VectorHit, error)
}

// RetrievalToolSelector 基于 FTS5 + 向量 RRF 的工具检索实现。
//
// 装配位置：cmd/harness-go/main.go（唯一同时可见 store 与 retrieval 的地方）。
type RetrievalToolSelector struct {
	// Repo 检索仓储（nil 时退化为「只用回退集」，绝不 panic）。
	Repo ToolSearchRepo
	// Kind 索引类别（工具固定为 retrieval.KindTool）。
	Kind retrieval.Kind
	// TopK 动态注入条数上限（推荐 4~8）。
	TopK int
	// MinTopK 降级允许的最小条数。
	MinTopK int
	// Embed 可选的查询向量化函数；为 nil 时只走关键词通道。
	Embed func(ctx context.Context, text string) ([]float32, error)
	// ModelID 与 Embed 配套的模型标识（向量通道按模型隔离）。
	ModelID string
	// Recent 最近使用工具名提供者（检索为空时的回退来源）。
	Recent func() []string
	// Always 核心集提供者（回退的最终兜底）。
	Always func() []string
	// HalveTopK 是否把 TopK 减半（预算闸门第二级降级开关）。
	HalveTopK bool
}

// SelectForPrompt 执行「关键词 + 向量 → RRF → TopK」并返回注入名单。
//
// 失败策略：任何检索通道出错都**降级为回退集而非报错** ——
// 检索是「优化项」，不能因为索引损坏就让整轮对话失败（宽进严出的可观测版）。
func (s *RetrievalToolSelector) SelectForPrompt(ctx context.Context, userInput string, candidates []prompt.ToolRef) []string {
	kind := s.Kind
	if !kind.Valid() {
		kind = retrieval.KindTool
	}
	opts := retrieval.SelectOptions{
		Query:        userInput,
		TopK:         retrieval.ClampTopK(retrieval.SelectOptions{TopK: s.TopK, MinTopK: s.MinTopK}, s.HalveTopK),
		MinTopK:      s.MinTopK,
		ExcludeNames: coreNames(candidates),
	}
	if s.Recent != nil {
		opts.RecentNames = s.Recent()
	}
	if s.Always != nil {
		opts.AlwaysNames = s.Always()
	}

	var hits []retrieval.FusedHit
	if s.Repo != nil && strings.TrimSpace(userInput) != "" {
		// 两路召回量取 TopK 的 3 倍，给 RRF 留出融合余量（太小会让 RRF 退化成单路）。
		fetch := opts.TopK * 3
		if fetch < 10 {
			fetch = 10
		}
		kw, err := s.Repo.SearchKeyword(ctx, kind, userInput, fetch)
		if err != nil {
			kw = nil // 关键词通道失败不阻断，继续尝试向量通道
		}
		var vec []retrieval.VectorHit
		if s.Embed != nil {
			if q, eerr := s.Embed(ctx, userInput); eerr == nil && len(q) > 0 {
				if vh, verr := s.Repo.SearchVector(ctx, kind, q, s.ModelID, fetch); verr == nil {
					vec = vh
				}
			}
		}
		hits = retrieval.RRF(kw, vec, retrieval.DefaultRRFK)
	}

	return retrieval.Names(retrieval.SelectTools(hits, opts))
}

// coreNames 提取常驻集工具名（core 档不进动态检索候选，避免重复注入）。
func coreNames(candidates []prompt.ToolRef) []string {
	out := make([]string, 0, len(candidates))
	for _, t := range candidates {
		if t.Tier == prompt.TierCore {
			out = append(out, t.Name)
		}
	}
	return out
}
