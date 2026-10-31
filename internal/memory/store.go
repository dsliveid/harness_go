package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"harness-go/internal/retrieval"
	"harness-go/internal/tokenizer"
)

// Repo 记忆仓储窄端口。
//
// 方法签名与 *store.RetrievalRepo 逐一对应（装配点直接传入，零适配器）；
// 在此定义窄接口是为了测试可注入假实现（与 agent/ports.go 的做法一致）。
type Repo interface {
	UpsertDoc(ctx context.Context, doc retrieval.Doc) (int64, bool, error)
	GetDoc(ctx context.Context, kind retrieval.Kind, key string) (*retrieval.Doc, error)
	DeleteDoc(ctx context.Context, kind retrieval.Kind, key string) error
	ListKeys(ctx context.Context, kind retrieval.Kind) ([]string, error)
	TouchRef(ctx context.Context, docID int64, at int64) error
	Count(ctx context.Context, kind retrieval.Kind) (int, error)
	SearchKeyword(ctx context.Context, kind retrieval.Kind, query string, topN int) ([]retrieval.KeywordHit, error)
	SearchVector(ctx context.Context, kind retrieval.Kind, query []float32, modelID string, topN int) ([]retrieval.VectorHit, error)
	PutVector(ctx context.Context, docID int64, modelID string, vec []float32) error
}

// L0MaxTokens L0 核心层注入上限（token）。超出时按写入时间从旧到新丢弃尾部条目。
const L0MaxTokens = 800

// L1SummaryLines L1 项目层注入的摘要行数。
const L1SummaryLines = 3

// Entry 写入条目。
type Entry struct {
	Layer      Layer
	Key        string // 唯一键（同层内）：如 "pref:输出语言"、"conv:<sid>:<n>"、文件相对路径
	Title      string // 一行标题（进 FTS5）
	Body       string // 正文（进 FTS5）
	Importance float64
}

// Store 记忆四层存储：写入（四分类去重）+ 召回（打分注入块）。
type Store struct {
	repo    Repo
	embed   func(ctx context.Context, text string) ([]float32, error)
	modelID string
	now     func() int64
	counter tokenizer.Counter
}

// Options 构造参数。Repo 必填；Embed 为 nil 时相似度判定退化为「同键同文判重 +
// 关键词通道召回」，向量通道自动停用（诚实降级，见 score.go 注释）。
type Options struct {
	Repo    Repo
	Embed   func(ctx context.Context, text string) ([]float32, error)
	ModelID string
	// Now 时间源（测试注入）；nil 用 time.Now().UnixMilli()。
	Now func() int64
	// Counter L0 上限计数器；nil 用 heuristic 估算。
	Counter tokenizer.Counter
}

// New 创建记忆存储。
func New(opts Options) *Store {
	now := opts.Now
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	counter := opts.Counter
	if counter == nil {
		counter = tokenizer.NewHeuristic()
	}
	return &Store{repo: opts.Repo, embed: opts.Embed, modelID: opts.ModelID, now: now, counter: counter}
}

// Write 写入一条记忆：先四分类去重，再按决策落库。
//
// 决策语义（严格对应 Classify）：
//   - 同键同文（幂等重写）→ NOOP（UpsertDoc 幂等路径只刷新 last_ref_at）；
//   - 同键异文 → UPDATE（调用方显式重写该键，直接覆盖，不做相似度拦截）；
//   - 异键且首位相似度 > 0.92 → NOOP（重复事实，刷新旧条目 last_ref_at）；
//   - 异键且首位相似度 > 0.80 → UPDATE（新内容**覆盖到旧条目的键上**，
//     旧键存活、内容被替换 —— mem0 语义：更新的是「那条记忆」，不是新建）；
//   - 其余 → ADD。
//
// DELETE 不在本方法自动触发：矛盾判定需要语义理解方，由上层判定后显式调 Delete
// 再 Write（先删后写），本层绝不臆测删数据。
//
// 返回 (实际操作, 被覆盖条目的键（仅 UPDATE 异键时非空）, error)。
func (s *Store) Write(ctx context.Context, e Entry) (Op, string, error) {
	if s.repo == nil {
		return "", "", fmt.Errorf("memory: 缺少仓储依赖")
	}
	if !e.Layer.Valid() {
		return "", "", fmt.Errorf("memory: 记忆层非法: %d", int(e.Layer))
	}
	doc, ok := (retrieval.Doc{
		Kind:       e.Layer.Kind(),
		Key:        e.Key,
		Title:      e.Title,
		Body:       e.Body,
		Importance: e.Importance,
	}).Normalize()
	if !ok {
		return "", "", fmt.Errorf("memory: 条目非法（key 不能为空）")
	}
	now := s.now()

	// ① 同键判定：存在即由调用方显式意图接管（同文幂等 / 异文覆盖）。
	existing, err := s.repo.GetDoc(ctx, doc.Kind, doc.Key)
	if err != nil {
		return "", "", fmt.Errorf("memory: 读取既有条目失败: %w", err)
	}
	if existing != nil {
		if existing.ContentHash == doc.ContentHash {
			if _, _, uerr := s.repo.UpsertDoc(ctx, doc); uerr != nil { // 幂等路径：刷 last_ref_at
				return "", "", fmt.Errorf("memory: 刷新条目失败: %w", uerr)
			}
			return OpNoop, "", nil
		}
		id, _, uerr := s.repo.UpsertDoc(ctx, doc)
		if uerr != nil {
			return "", "", fmt.Errorf("memory: 覆盖条目失败: %w", uerr)
		}
		if err := s.writeVector(ctx, id, doc.Body); err != nil {
			return "", "", err
		}
		return OpUpdate, "", nil // 同键覆盖：调用方已知键，无需报告
	}

	// ② 异键相似度判定（无 embedding 时无可比向量 → 直接 ADD，诚实降级）。
	similar, err := s.findSimilar(ctx, doc, now)
	if err != nil {
		return "", "", err
	}
	switch Classify(similar, false) {
	case OpNoop:
		// 重复事实：刷新被命中条目的召回时间（时间衰减重新起算），不写新行。
		if err := s.repo.TouchRef(ctx, similar[0].DocID, now); err != nil {
			return "", "", fmt.Errorf("memory: 刷新重复条目召回时间失败: %w", err)
		}
		return OpNoop, "", nil
	case OpUpdate:
		// 覆盖到旧条目的键上：旧键存活、内容替换（该键的向量随之重写）。
		old := similar[0]
		merged := doc
		merged.Key = old.Key
		id, _, uerr := s.repo.UpsertDoc(ctx, merged)
		if uerr != nil {
			return "", "", fmt.Errorf("memory: 覆盖相似条目失败: %w", uerr)
		}
		if err := s.writeVector(ctx, id, merged.Body); err != nil {
			return "", "", err
		}
		return OpUpdate, old.Key, nil
	default: // OpAdd
		id, _, aerr := s.repo.UpsertDoc(ctx, doc)
		if aerr != nil {
			return "", "", fmt.Errorf("memory: 写入条目失败: %w", aerr)
		}
		if err := s.writeVector(ctx, id, doc.Body); err != nil {
			return "", "", err
		}
		return OpAdd, "", nil
	}
}

// Delete 显式删除一条记忆（矛盾/过期清理由上层语义判定后调用）。
func (s *Store) Delete(ctx context.Context, layer Layer, key string) error {
	if s.repo == nil {
		return fmt.Errorf("memory: 缺少仓储依赖")
	}
	if !layer.Valid() {
		return fmt.Errorf("memory: 记忆层非法: %d", int(layer))
	}
	return s.repo.DeleteDoc(ctx, layer.Kind(), key)
}

// ScoredHit 召回命中（含打分明细，供可观测与测试断言）。
type ScoredHit struct {
	Layer      Layer
	Key        string
	Title      string
	Body       string
	Score      float64
	Cos        float64
	Decay      float64
	Importance float64
	DocID      int64
}

// Recall 四层召回结果：Core 进 system 稳定前缀，Tail 置尾（动态内容）。
type Recall struct {
	Core    string      // L0 全量（≤800 token）+ L1 三行摘要；两层全空时为空串
	Tail    string      // L2/L3 打分命中（各自 ≥ MinRecallHits 才注入该层）
	L0Count int         // 实际注入的 L0 条数（超上限被丢弃的不计）
	L1Count int         // L1 条数
	Hits    []ScoredHit // 注入的命中（L2 在前，层内按分数降序）
}

// RecallBlocks 便捷封装：直接返回注入块文本（service.MemorySource 端口形状）。
func (s *Store) RecallBlocks(ctx context.Context, query string) (core, tail string, err error) {
	r, err := s.Recall(ctx, query)
	if err != nil {
		return "", "", err
	}
	return r.Core, r.Tail, nil
}

// Recall 执行四层召回。query 为空时只返回常驻块（L0/L1），不做 L2/L3 检索
// （没有查询就没有相关性，硬凑 top-k 只会注入噪声）。
func (s *Store) Recall(ctx context.Context, query string) (Recall, error) {
	if s.repo == nil {
		return Recall{}, fmt.Errorf("memory: 缺少仓储依赖")
	}
	now := s.now()
	var out Recall

	// ---- L0 核心常驻（≤800 token，写入顺序从旧到新，超出预算的尾部条目丢弃）----
	l0, err := s.listLayer(ctx, L0)
	if err != nil {
		return out, err
	}
	var coreParts []string
	used := 0
	for _, d := range l0 {
		t := s.counter.CountText(d.Body)
		if used+t > L0MaxTokens {
			break
		}
		coreParts = append(coreParts, d.Body)
		used += t
		out.L0Count++
	}

	// ---- L1 项目摘要（前 3 行非空文本）----
	l1, err := s.listLayer(ctx, L1)
	if err != nil {
		return out, err
	}
	out.L1Count = len(l1)
	if len(l1) > 0 {
		var lines []string
		for _, d := range l1 {
			for _, ln := range strings.Split(d.Body, "\n") {
				if t := strings.TrimSpace(ln); t != "" {
					lines = append(lines, t)
					if len(lines) >= L1SummaryLines {
						break
					}
				}
			}
			if len(lines) >= L1SummaryLines {
				break
			}
		}
		if len(lines) > 0 {
			coreParts = append(coreParts, "项目记忆摘要："+strings.Join(lines, "；"))
		}
	}
	out.Core = strings.Join(coreParts, "\n")

	// ---- L2/L3 检索（打分 top-k，命中不足整层不注入）----
	if strings.TrimSpace(query) == "" {
		return out, nil
	}
	for _, layer := range []Layer{L2, L3} {
		hits, err := s.searchLayer(ctx, layer, query, now)
		if err != nil {
			return out, err
		}
		if len(hits) < MinRecallHits {
			continue // 宁缺毋滥
		}
		out.Hits = append(out.Hits, hits...)
		// 召回即刷新 last_ref_at：下次检索时这些条目的时间衰减重新起算
		// （best-effort：刷新失败不影响本轮注入）。
		for _, h := range hits {
			_ = s.repo.TouchRef(ctx, h.DocID, now)
		}
	}
	out.Tail = renderTail(out.Hits)
	return out, nil
}

// findSimilar 取同层既有条目中与新条目最相似的前 5 条（降序）。
// 无 embedding 时返回空（调用方按 ADD 处理）。
func (s *Store) findSimilar(ctx context.Context, doc retrieval.Doc, now int64) ([]Similar, error) {
	if s.embed == nil {
		return nil, nil
	}
	vec, err := s.embed(ctx, doc.Body)
	if err != nil || len(vec) == 0 {
		// 向量化失败 = 无可比向量：按 ADD 处理（宁多写不漏写，去重是优化不是正确性）
		return nil, nil
	}
	hits, err := s.repo.SearchVector(ctx, doc.Kind, vec, s.modelID, 5)
	if err != nil {
		return nil, fmt.Errorf("memory: 相似度检索失败: %w", err)
	}
	out := make([]Similar, 0, len(hits))
	for _, h := range hits {
		out = append(out, Similar{Key: h.Key, DocID: h.DocID, Score: h.Score})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

// writeVector 写入条目向量（best-effort：无 embedding 或向量化失败时跳过 ——
// 向量是召回优化项，缺向量只影响相似度精度，不阻断写入）。
func (s *Store) writeVector(ctx context.Context, docID int64, body string) error {
	if s.embed == nil {
		return nil
	}
	vec, err := s.embed(ctx, body)
	if err != nil || len(vec) == 0 {
		return nil
	}
	if perr := s.repo.PutVector(ctx, docID, s.modelID, vec); perr != nil {
		return fmt.Errorf("memory: 写入条目向量失败: %w", perr)
	}
	return nil
}

// listLayer 列出某层全部条目（按 updated_at 升序 = 写入顺序）。
func (s *Store) listLayer(ctx context.Context, layer Layer) ([]retrieval.Doc, error) {
	keys, err := s.repo.ListKeys(ctx, layer.Kind())
	if err != nil {
		return nil, fmt.Errorf("memory: 列出 %s 条目失败: %w", layer, err)
	}
	out := make([]retrieval.Doc, 0, len(keys))
	for _, k := range keys {
		d, err := s.repo.GetDoc(ctx, layer.Kind(), k)
		if err != nil {
			return nil, fmt.Errorf("memory: 读取 %s 条目 %s 失败: %w", layer, k, err)
		}
		if d != nil {
			out = append(out, *d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt < out[j].UpdatedAt })
	return out, nil
}

// searchLayer 单层检索：关键词 ∪ 向量候选 → 逐条打分 → 降序截断。
func (s *Store) searchLayer(ctx context.Context, layer Layer, query string, now int64) ([]ScoredHit, error) {
	const fetchN = 20
	kind := layer.Kind()
	cand := make(map[string]retrieval.Doc) // key → doc（GetDoc 结果缓存）

	kw, err := s.repo.SearchKeyword(ctx, kind, query, fetchN)
	if err != nil {
		return nil, fmt.Errorf("memory: %s 关键词检索失败: %w", layer, err)
	}
	for _, h := range kw {
		if _, ok := cand[h.Key]; ok {
			continue
		}
		d, gerr := s.repo.GetDoc(ctx, kind, h.Key)
		if gerr != nil || d == nil {
			continue
		}
		cand[h.Key] = *d
	}

	cosOf := make(map[string]float64) // key → 与查询的余弦（无向量记 0）
	if s.embed != nil {
		if qvec, qerr := s.embed(ctx, query); qerr == nil && len(qvec) > 0 {
			vh, verr := s.repo.SearchVector(ctx, kind, qvec, s.modelID, fetchN)
			if verr != nil {
				return nil, fmt.Errorf("memory: %s 向量检索失败: %w", layer, err)
			}
			for _, h := range vh {
				cosOf[h.Key] = h.Score
				if _, ok := cand[h.Key]; ok {
					continue
				}
				d, gerr := s.repo.GetDoc(ctx, kind, h.Key)
				if gerr != nil || d == nil {
					continue
				}
				cand[h.Key] = *d
			}
		}
	}

	hits := make([]ScoredHit, 0, len(cand))
	for key, d := range cand {
		cos := cosOf[key]
		dec := Decay(d.LastRefAt, now)
		hits = append(hits, ScoredHit{
			Layer:      layer,
			Key:        d.Key,
			Title:      d.Title,
			Body:       d.Body,
			Cos:        cos,
			Decay:      dec,
			Importance: d.Importance,
			Score:      Score(cos, d.LastRefAt, now, d.Importance),
			DocID:      d.ID,
		})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Key < hits[j].Key // 同分按键名升序（确定性排序）
	})
	if len(hits) > RecallTopK {
		hits = hits[:RecallTopK]
	}
	return hits, nil
}

// renderTail 把命中渲染为置尾文本块（无命中返回空串）。
func renderTail(hits []ScoredHit) string {
	if len(hits) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## 相关记忆（自动召回，供参考）\n")
	for _, h := range hits {
		body := firstMemoryLine(h.Body)
		if h.Layer == L3 {
			// L3 档案条目带来源引用（key = 文件相对路径），便于 path:line 回溯
			fmt.Fprintf(&sb, "- [%s] %s（来源 %s）：%s\n", h.Layer, h.Title, h.Key, body)
			continue
		}
		fmt.Fprintf(&sb, "- [%s] %s：%s\n", h.Layer, h.Title, body)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// firstMemoryLine 取正文首行做摘要（按 rune 截断到 80，防止单条命中吃掉尾段预算）。
func firstMemoryLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return s
}
