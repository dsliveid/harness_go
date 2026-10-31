package memory

import (
	"context"
	"strings"
	"testing"

	"harness-go/internal/retrieval"
)

// ============ fake repo（端口零适配的直接验证） ============

type fakeRepo struct {
	docs   map[string]retrieval.Doc // kind|key → doc
	nextID int64
	touch  []int64
	// vectorScores 测试注入的「既有条目对当前查询的余弦」（key → score），
	// 直接控制 findSimilar / searchLayer 的相似度输入。
	vectorScores map[string]float64
	// queryVecs 记录 SearchVector 收到的查询向量（断言向量通道被调用）。
	queryVecs [][]float32
	failGet   bool
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{docs: map[string]retrieval.Doc{}, nextID: 100, vectorScores: map[string]float64{}}
}

func fkey(kind retrieval.Kind, key string) string { return string(kind) + "|" + key }

func (f *fakeRepo) UpsertDoc(_ context.Context, doc retrieval.Doc) (int64, bool, error) {
	norm, ok := doc.Normalize()
	if !ok {
		return 0, false, retrieval.ErrEmptyVector
	}
	old, exists := f.docs[fkey(norm.Kind, norm.Key)]
	if exists && old.ContentHash == norm.ContentHash {
		old.LastRefAt = norm.LastRefAt
		f.docs[fkey(norm.Kind, norm.Key)] = old
		return old.ID, false, nil
	}
	if exists {
		norm.ID = old.ID
	} else {
		f.nextID++
		norm.ID = f.nextID
	}
	if norm.UpdatedAt == 0 {
		norm.UpdatedAt = 1000
	}
	if norm.LastRefAt == 0 {
		norm.LastRefAt = 1000
	}
	f.docs[fkey(norm.Kind, norm.Key)] = norm
	return norm.ID, true, nil
}

func (f *fakeRepo) GetDoc(_ context.Context, kind retrieval.Kind, key string) (*retrieval.Doc, error) {
	if f.failGet {
		return nil, retrieval.ErrEmptyVector
	}
	if d, ok := f.docs[fkey(kind, key)]; ok {
		cp := d
		return &cp, nil
	}
	return nil, nil
}

func (f *fakeRepo) DeleteDoc(_ context.Context, kind retrieval.Kind, key string) error {
	delete(f.docs, fkey(kind, key))
	return nil
}

func (f *fakeRepo) ListKeys(_ context.Context, kind retrieval.Kind) ([]string, error) {
	var out []string
	for _, d := range f.docs {
		if d.Kind == kind {
			out = append(out, d.Key)
		}
	}
	return out, nil
}

func (f *fakeRepo) TouchRef(_ context.Context, docID int64, at int64) error {
	f.touch = append(f.touch, docID)
	for k, d := range f.docs {
		if d.ID == docID {
			d.LastRefAt = at
			f.docs[k] = d
		}
	}
	return nil
}
func (f *fakeRepo) Count(_ context.Context, kind retrieval.Kind) (int, error) {
	n := 0
	for _, d := range f.docs {
		if d.Kind == kind {
			n++
		}
	}
	return n, nil
}

func (f *fakeRepo) SearchKeyword(_ context.Context, kind retrieval.Kind, query string, topN int) ([]retrieval.KeywordHit, error) {
	var out []retrieval.KeywordHit
	for _, d := range f.docs {
		if d.Kind != kind {
			continue
		}
		if strings.Contains(d.Body, strings.TrimSpace(query)) {
			out = append(out, retrieval.KeywordHit{DocID: d.ID, Key: d.Key, BM25: -1, Rank: len(out) + 1})
		}
		if len(out) >= topN {
			break
		}
	}
	return out, nil
}

func (f *fakeRepo) SearchVector(_ context.Context, kind retrieval.Kind, query []float32, _ string, topN int) ([]retrieval.VectorHit, error) {
	f.queryVecs = append(f.queryVecs, query)
	var out []retrieval.VectorHit
	for _, d := range f.docs {
		if d.Kind != kind {
			continue
		}
		if s, ok := f.vectorScores[d.Key]; ok {
			out = append(out, retrieval.VectorHit{DocID: d.ID, Key: d.Key, Score: s})
		}
		if len(out) >= topN {
			break
		}
	}
	return out, nil
}

func (f *fakeRepo) PutVector(_ context.Context, _ int64, _ string, _ []float32) error { return nil }

func testStore(r Repo, now int64) *Store {
	return New(Options{
		Repo: r,
		Embed: func(context.Context, string) ([]float32, error) {
			return []float32{1}, nil
		},
		Now: func() int64 { return now },
	})
}

// ============ Write 四分类编排 ============

func TestWriteSameKeySameContentNoop(t *testing.T) {
	r := newFakeRepo()
	s := testStore(r, 1000)
	op, _, err := s.Write(context.Background(), Entry{Layer: L2, Key: "k1", Title: "t", Body: "用户偏好深色主题"})
	if err != nil || op != OpAdd {
		t.Fatalf("首次写入应 ADD: op=%v err=%v", op, err)
	}
	op, _, err = s.Write(context.Background(), Entry{Layer: L2, Key: "k1", Title: "t", Body: "用户偏好深色主题"})
	if err != nil || op != OpNoop {
		t.Fatalf("同键同文应 NOOP（幂等）: op=%v err=%v", op, err)
	}
	if n, _ := r.Count(context.Background(), L2.Kind()); n != 1 {
		t.Fatalf("幂等重写不得新增行，实际 %d 条", n)
	}
}

func TestWriteSameKeyDiffContentUpdate(t *testing.T) {
	r := newFakeRepo()
	s := testStore(r, 1000)
	if _, _, err := s.Write(context.Background(), Entry{Layer: L0, Key: "pref", Body: "输出用中文"}); err != nil {
		t.Fatal(err)
	}
	op, overwritten, err := s.Write(context.Background(), Entry{Layer: L0, Key: "pref", Body: "输出用英文"})
	if err != nil || op != OpUpdate {
		t.Fatalf("同键异文应 UPDATE: op=%v err=%v", op, err)
	}
	if overwritten != "" {
		t.Fatalf("同键覆盖不产生被覆盖键: %q", overwritten)
	}
	d, _ := r.GetDoc(context.Background(), L0.Kind(), "pref")
	if d == nil || d.Body != "输出用英文" {
		t.Fatalf("覆盖后内容应为新文: %+v", d)
	}
}

func TestWriteHighSimilarityNoopAndTouchesRef(t *testing.T) {
	r := newFakeRepo()
	s := testStore(r, 1000)
	if _, _, err := s.Write(context.Background(), Entry{Layer: L2, Key: "old", Body: "部署用 docker compose"}); err != nil {
		t.Fatal(err)
	}
	oldID := r.docs[fkey(L2.Kind(), "old")].ID
	r.vectorScores["old"] = 0.95 // 新内容与旧条目相似度 0.95 > 0.92

	op, overwritten, err := s.Write(context.Background(), Entry{Layer: L2, Key: "new", Body: "部署用 docker compose v2"})
	if err != nil || op != OpNoop {
		t.Fatalf("相似度 0.95 应 NOOP: op=%v err=%v", op, err)
	}
	if overwritten != "" {
		t.Fatalf("NOOP 不产生覆盖键")
	}
	if n, _ := r.Count(context.Background(), L2.Kind()); n != 1 {
		t.Fatalf("重复事实禁止重复落库，实际 %d 条", n)
	}
	if len(r.touch) != 1 || r.touch[0] != oldID {
		t.Fatalf("NOOP 应刷新旧条目 last_ref_at: %v", r.touch)
	}
}

func TestWriteUpdateBandOverwritesOldKey(t *testing.T) {
	r := newFakeRepo()
	s := testStore(r, 1000)
	if _, _, err := s.Write(context.Background(), Entry{Layer: L2, Key: "old", Title: "旧", Body: "测试端口 8080"}); err != nil {
		t.Fatal(err)
	}
	r.vectorScores["old"] = 0.85 // 0.80~0.92：同一事实的新表述

	op, overwritten, err := s.Write(context.Background(), Entry{Layer: L2, Key: "new", Body: "测试端口 9090"})
	if err != nil || op != OpUpdate {
		t.Fatalf("相似度 0.85 应 UPDATE: op=%v err=%v", op, err)
	}
	if overwritten != "old" {
		t.Fatalf("UPDATE 应报告被覆盖的旧键: %q", overwritten)
	}
	// 覆盖语义：旧键存活、内容替换；调用方的新键不落库
	if d, _ := r.GetDoc(context.Background(), L2.Kind(), "old"); d == nil || d.Body != "测试端口 9090" {
		t.Fatalf("旧键应承载新内容: %+v", d)
	}
	if d, _ := r.GetDoc(context.Background(), L2.Kind(), "new"); d != nil {
		t.Fatalf("新键不得落库（否则同一事实出现两行）")
	}
}

func TestWriteLowSimilarityAdd(t *testing.T) {
	r := newFakeRepo()
	s := testStore(r, 1000)
	if _, _, err := s.Write(context.Background(), Entry{Layer: L3, Key: "a", Body: "事实一"}); err != nil {
		t.Fatal(err)
	}
	r.vectorScores["a"] = 0.3
	op, _, err := s.Write(context.Background(), Entry{Layer: L3, Key: "b", Body: "事实二"})
	if err != nil || op != OpAdd {
		t.Fatalf("低相似度应 ADD: op=%v err=%v", op, err)
	}
	if n, _ := r.Count(context.Background(), L3.Kind()); n != 2 {
		t.Fatalf("应落库 2 条，实际 %d", n)
	}
}

func TestWriteInvalidEntry(t *testing.T) {
	s := testStore(newFakeRepo(), 1000)
	if _, _, err := s.Write(context.Background(), Entry{Layer: L0, Key: "  ", Body: "x"}); err == nil {
		t.Fatalf("空键应报错")
	}
	if _, _, err := s.Write(context.Background(), Entry{Layer: Layer(9), Key: "k", Body: "x"}); err == nil {
		t.Fatalf("非法层应报错")
	}
}

// ============ Recall 四层注入块 ============

func TestRecallL0BudgetCap(t *testing.T) {
	r := newFakeRepo()
	s := testStore(r, 1000)
	ctx := context.Background()
	// 写入 3 条，每条约 600 token（heuristic 按 CJK 字符计），只能装下 1 条
	for i, body := range []string{strings.Repeat("甲", 600), strings.Repeat("乙", 600), strings.Repeat("丙", 600)} {
		if _, _, err := s.Write(ctx, Entry{Layer: L0, Key: string(rune('a' + i)), Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	rec, err := s.Recall(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if rec.L0Count != 1 || !strings.Contains(rec.Core, strings.Repeat("甲", 10)) {
		t.Fatalf("L0 应按预算只注入最早 1 条: count=%d core=%d 字", rec.L0Count, len(rec.Core))
	}
	if strings.Contains(rec.Core, "乙") || strings.Contains(rec.Core, "丙") {
		t.Fatalf("超出预算的条目不得注入")
	}
}

func TestRecallL1ThreeLineSummary(t *testing.T) {
	r := newFakeRepo()
	s := testStore(r, 1000)
	ctx := context.Background()
	if _, _, err := s.Write(ctx, Entry{Layer: L1, Key: "p", Body: "技术栈 Go+React\n约定：LF 换行\n避坑：别用 CGO\n第四行不应出现"}); err != nil {
		t.Fatal(err)
	}
	rec, err := s.Recall(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Core, "技术栈 Go+React") || !strings.Contains(rec.Core, "约定：LF 换行") {
		t.Fatalf("摘要应含前两行: %q", rec.Core)
	}
	if strings.Contains(rec.Core, "第四行不应出现") {
		t.Fatalf("摘要只取 3 行")
	}
}

func TestRecallL2MinHitsRule(t *testing.T) {
	r := newFakeRepo()
	s := testStore(r, 1000)
	ctx := context.Background()
	// 只有 2 条 L2 命中（< MinRecallHits=3）→ 整层不注入
	for i := 0; i < 2; i++ {
		if _, _, err := s.Write(ctx, Entry{Layer: L2, Key: string(rune('a' + i)), Body: "结论包含查询词"}); err != nil {
			t.Fatal(err)
		}
	}
	r.vectorScores["a"] = 0.9
	r.vectorScores["b"] = 0.8
	rec, err := s.Recall(ctx, "查询词")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Tail != "" {
		t.Fatalf("命中 %d 条（<3）不得注入（宁缺毋滥）: %q", len(rec.Hits), rec.Tail)
	}
	// 补第 3 条：先清相似度表再写（否则 0.9 相似度会把它拦成 UPDATE 而非新增）
	r.vectorScores = map[string]float64{}
	if _, _, err := s.Write(ctx, Entry{Layer: L2, Key: "c", Body: "结论包含查询词"}); err != nil {
		t.Fatal(err)
	}
	r.vectorScores["a"] = 0.9
	r.vectorScores["b"] = 0.8
	rec, err = s.Recall(ctx, "查询词")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Hits) != 3 || rec.Tail == "" {
		t.Fatalf("命中 3 条应整层注入: hits=%d tail=%q", len(rec.Hits), rec.Tail)
	}
	// 分数降序：a(0.9) > b(0.8) > c(0)
	if rec.Hits[0].Key != "a" || rec.Hits[1].Key != "b" || rec.Hits[2].Key != "c" {
		t.Fatalf("应按打分降序: %+v", rec.Hits)
	}
}

func TestRecallL3SourceReference(t *testing.T) {
	r := newFakeRepo()
	s := testStore(r, 1000)
	ctx := context.Background()
	for _, k := range []string{"docs/a.md", "docs/b.md", "docs/c.md"} {
		if _, _, err := s.Write(ctx, Entry{Layer: L3, Key: k, Title: "档案", Body: "档案内容含查询词"}); err != nil {
			t.Fatal(err)
		}
	}
	rec, err := s.Recall(ctx, "查询词")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Tail, "来源 docs/a.md") {
		t.Fatalf("L3 命中必须带来源引用（path 回溯）: %q", rec.Tail)
	}
}

func TestRecallTouchesInjectedDocs(t *testing.T) {
	r := newFakeRepo()
	s := testStore(r, 1000)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, _, err := s.Write(ctx, Entry{Layer: L2, Key: string(rune('a' + i)), Body: "内容包含查询词"}); err != nil {
			t.Fatal(err)
		}
	}
	r.touch = nil
	if _, err := s.Recall(ctx, "查询词"); err != nil {
		t.Fatal(err)
	}
	if len(r.touch) != 3 {
		t.Fatalf("注入的命中应刷新 last_ref_at（时间衰减重算），实际 %v", r.touch)
	}
}

func TestRecallEmptyQueryCoreOnly(t *testing.T) {
	r := newFakeRepo()
	s := testStore(r, 1000)
	ctx := context.Background()
	if _, _, err := s.Write(ctx, Entry{Layer: L0, Key: "k", Body: "核心约束"}); err != nil {
		t.Fatal(err)
	}
	vecCalls := len(r.queryVecs)
	rec, err := s.Recall(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Core == "" || rec.Tail != "" || len(rec.Hits) != 0 {
		t.Fatalf("空查询只返回常驻块: %+v", rec)
	}
	if len(r.queryVecs) != vecCalls {
		t.Fatalf("空查询不得触发向量检索")
	}
}

func TestRecallRepoErrorPropagates(t *testing.T) {
	r := newFakeRepo()
	s := testStore(r, 1000)
	ctx := context.Background()
	if _, _, err := s.Write(ctx, Entry{Layer: L0, Key: "k", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	r.failGet = true // 写入成功后人为破坏读取，模拟仓储故障
	if _, err := s.Recall(ctx, ""); err == nil {
		t.Fatalf("仓储错误必须上抛（调用方决定降级），不得静默吞掉")
	}
}

func TestDeleteExplicit(t *testing.T) {
	r := newFakeRepo()
	s := testStore(r, 1000)
	ctx := context.Background()
	if _, _, err := s.Write(ctx, Entry{Layer: L2, Key: "k", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, L2, "k"); err != nil {
		t.Fatal(err)
	}
	if n, _ := r.Count(ctx, L2.Kind()); n != 0 {
		t.Fatalf("删除后应为 0 条，实际 %d", n)
	}
}
