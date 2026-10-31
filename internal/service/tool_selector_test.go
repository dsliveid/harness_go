package service

import (
	"context"
	"errors"
	"testing"

	"harness-go/internal/prompt"
	"harness-go/internal/retrieval"
)

// fakeSearchRepo 可控的检索仓储桩（用于验证两条通道的失败降级）。
type fakeSearchRepo struct {
	kwHits  []retrieval.KeywordHit
	kwErr   error
	vecHits []retrieval.VectorHit
	vecErr  error
	// 记录调用情况，验证「关键词失败仍继续向量通道」。
	kwCalled  bool
	vecCalled bool
}

func (f *fakeSearchRepo) SearchKeyword(_ context.Context, _ retrieval.Kind, _ string, _ int) ([]retrieval.KeywordHit, error) {
	f.kwCalled = true
	return f.kwHits, f.kwErr
}

func (f *fakeSearchRepo) SearchVector(_ context.Context, _ retrieval.Kind, _ []float32, _ string, _ int) ([]retrieval.VectorHit, error) {
	f.vecCalled = true
	return f.vecHits, f.vecErr
}

func deferredCandidates() []prompt.ToolRef {
	return []prompt.ToolRef{
		{Name: "read_file", Tier: prompt.TierCore},
		{Name: "plan", Tier: prompt.TierDeferred},
		{Name: "web_fetch", Tier: prompt.TierDeferred},
	}
}

// 关键词通道命中：返回命中工具名（core 档被排除，避免重复注入）。
func TestSelectorKeywordHit(t *testing.T) {
	repo := &fakeSearchRepo{
		kwHits: []retrieval.KeywordHit{{DocID: 1, Key: "web_fetch", BM25: -1.0, Rank: 1}},
	}
	s := &RetrievalToolSelector{Repo: repo, Kind: retrieval.KindTool, TopK: 4}
	got := s.SelectForPrompt(context.Background(), "抓取网页", deferredCandidates())

	if len(got) != 1 || got[0] != "web_fetch" {
		t.Fatalf("期望命中 web_fetch，得到 %+v", got)
	}
	if !repo.kwCalled {
		t.Fatal("关键词通道应被调用")
	}
}

// core 档工具即使被检索命中也不得进入动态注入名单（已在常驻集）。
func TestSelectorExcludesCoreTools(t *testing.T) {
	repo := &fakeSearchRepo{
		kwHits: []retrieval.KeywordHit{{DocID: 1, Key: "read_file", Rank: 1}},
	}
	s := &RetrievalToolSelector{Repo: repo, Kind: retrieval.KindTool, TopK: 4}
	got := s.SelectForPrompt(context.Background(), "读文件", deferredCandidates())

	if len(got) != 0 {
		t.Fatalf("core 档应被排除，得到 %+v", got)
	}
}

// 关键词通道报错不得阻断：应继续尝试向量通道。
func TestSelectorKeywordErrorFallsThroughToVector(t *testing.T) {
	repo := &fakeSearchRepo{
		kwErr:   errors.New("fts5 索引损坏"),
		vecHits: []retrieval.VectorHit{{DocID: 2, Key: "plan", Score: 0.9}},
	}
	s := &RetrievalToolSelector{
		Repo:  repo,
		Kind:  retrieval.KindTool,
		TopK:  4,
		Embed: func(context.Context, string) ([]float32, error) { return []float32{1, 0}, nil },
	}
	got := s.SelectForPrompt(context.Background(), "制定计划", deferredCandidates())

	if !repo.vecCalled {
		t.Fatal("关键词失败后仍应尝试向量通道")
	}
	if len(got) != 1 || got[0] != "plan" {
		t.Fatalf("应由向量通道召回 plan，得到 %+v", got)
	}
}

// Embed 失败只跳过向量通道，不影响关键词结果（检索是优化项，不阻断）。
func TestSelectorEmbedErrorKeepsKeyword(t *testing.T) {
	repo := &fakeSearchRepo{
		kwHits: []retrieval.KeywordHit{{DocID: 1, Key: "web_fetch", Rank: 1}},
	}
	s := &RetrievalToolSelector{
		Repo:  repo,
		Kind:  retrieval.KindTool,
		TopK:  4,
		Embed: func(context.Context, string) ([]float32, error) { return nil, errors.New("embed 服务不可用") },
	}
	got := s.SelectForPrompt(context.Background(), "抓取网页", deferredCandidates())

	if repo.vecCalled {
		t.Fatal("Embed 失败时不应调用向量通道")
	}
	if len(got) != 1 || got[0] != "web_fetch" {
		t.Fatalf("应保留关键词结果，得到 %+v", got)
	}
}

// Repo 为 nil：退化为纯回退集，绝不 panic。
//
// 注意回退集必须是 **deferred 档**工具名：core 档已在常驻注入中，
// 由 ExcludeNames 排除（重复注入等于白付 token），故这里用 plan/web_fetch。
func TestSelectorNilRepoUsesFallback(t *testing.T) {
	s := &RetrievalToolSelector{
		TopK:   4,
		Recent: func() []string { return []string{"plan"} },
		Always: func() []string { return []string{"web_fetch"} },
	}
	got := s.SelectForPrompt(context.Background(), "任意输入", deferredCandidates())

	if len(got) != 2 {
		t.Fatalf("应回退 2 条，得到 %+v", got)
	}
	if got[0] != "plan" || got[1] != "web_fetch" {
		t.Fatalf("回退顺序应为 recent→always，得到 %+v", got)
	}
}

// 空输入：不检索，直接回退（避免空查询把索引全量扫出来）。
func TestSelectorEmptyQuerySkipsSearch(t *testing.T) {
	repo := &fakeSearchRepo{
		kwHits: []retrieval.KeywordHit{{DocID: 1, Key: "web_fetch", Rank: 1}},
	}
	s := &RetrievalToolSelector{
		Repo:   repo,
		Kind:   retrieval.KindTool,
		TopK:   4,
		Always: func() []string { return []string{"plan"} },
	}
	got := s.SelectForPrompt(context.Background(), "   ", deferredCandidates())

	if repo.kwCalled {
		t.Fatal("空查询不应触发检索")
	}
	if len(got) != 1 || got[0] != "plan" {
		t.Fatalf("应直接回退到 plan，得到 %+v", got)
	}
}

// HalveTopK：预算降级时 TopK 减半（不低于 MinTopK）。
func TestSelectorHalveTopK(t *testing.T) {
	repo := &fakeSearchRepo{
		kwHits: []retrieval.KeywordHit{
			{DocID: 1, Key: "t1", Rank: 1}, {DocID: 2, Key: "t2", Rank: 2},
			{DocID: 3, Key: "t3", Rank: 3}, {DocID: 4, Key: "t4", Rank: 4},
		},
	}
	s := &RetrievalToolSelector{Repo: repo, Kind: retrieval.KindTool, TopK: 4, MinTopK: 2, HalveTopK: true}
	got := s.SelectForPrompt(context.Background(), "查询", deferredCandidates())

	if len(got) != 2 {
		t.Fatalf("降级后应减半为 2 条，得到 %d：%+v", len(got), got)
	}
}

// 两路都命中同一文档时，RRF 融合后只出现一次。
func TestSelectorFusionDedup(t *testing.T) {
	repo := &fakeSearchRepo{
		kwHits:  []retrieval.KeywordHit{{DocID: 1, Key: "web_fetch", Rank: 1}},
		vecHits: []retrieval.VectorHit{{DocID: 1, Key: "web_fetch", Score: 0.95}},
	}
	s := &RetrievalToolSelector{
		Repo:  repo,
		Kind:  retrieval.KindTool,
		TopK:  4,
		Embed: func(context.Context, string) ([]float32, error) { return []float32{1, 0}, nil },
	}
	got := s.SelectForPrompt(context.Background(), "抓取网页", deferredCandidates())

	if len(got) != 1 || got[0] != "web_fetch" {
		t.Fatalf("双通道同文档应融合为 1 条，得到 %+v", got)
	}
}
