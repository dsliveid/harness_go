package store

import (
	"context"
	"math"
	"testing"

	"harness-go/internal/retrieval"
)

// newRetrievalStore 打开一个临时主库（迁移含 0003_retrieval）。
func newRetrievalStore(t *testing.T) (*MainStore, *RetrievalRepo) {
	t.Helper()
	ctx := context.Background()
	m, err := OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("OpenMain 失败: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, m.Retrieval()
}

// 0003 迁移：FTS5 虚表与四张检索表必须全部建成，且主库版本为 3。
func TestRetrievalMigration(t *testing.T) {
	ctx := context.Background()
	m, _ := newRetrievalStore(t)

	names := []string{"retrieval_docs", "retrieval_fts", "retrieval_vectors", "index_meta"}
	for _, n := range names {
		var cnt int
		if err := m.conn().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, n).Scan(&cnt); err != nil {
			t.Fatalf("查询 sqlite_master 失败: %v", err)
		}
		if cnt != 1 {
			t.Fatalf("表 %q 应存在，得到 %d", n, cnt)
		}
	}

	v, err := SchemaVersion(ctx, m.conn())
	if err != nil {
		t.Fatal(err)
	}
	if v != 5 {
		t.Fatalf("主库 schema 版本期望 5，得到 %d", v)
	}
}

// 幂等：同内容重复 Upsert 只刷新时间不重写；内容变化才 changed=true。
func TestUpsertDocIdempotent(t *testing.T) {
	ctx := context.Background()
	_, repo := newRetrievalStore(t)

	d := retrieval.Doc{Kind: retrieval.KindTool, Key: "read_file", Title: "读文件", Body: "读取工作区文件"}
	id1, changed1, err := repo.UpsertDoc(ctx, d)
	if err != nil {
		t.Fatalf("首次 Upsert 失败: %v", err)
	}
	if !changed1 {
		t.Fatal("首次插入应 changed=true")
	}

	// 同内容再写一次：必须 changed=false（幂等前提，避免每轮对话全量重写索引）。
	id2, changed2, err := repo.UpsertDoc(ctx, d)
	if err != nil {
		t.Fatalf("二次 Upsert 失败: %v", err)
	}
	if changed2 {
		t.Fatal("同内容重复写入应 changed=false")
	}
	if id1 != id2 {
		t.Fatalf("同 (kind,key) 应复用同一 id：%d vs %d", id1, id2)
	}

	// 内容变化：changed=true，且 id 不变（原地 UPDATE）。
	d.Body = "读取工作区文件（支持分页）"
	id3, changed3, err := repo.UpsertDoc(ctx, d)
	if err != nil {
		t.Fatalf("内容变更 Upsert 失败: %v", err)
	}
	if !changed3 {
		t.Fatal("内容变化应 changed=true")
	}
	if id3 != id1 {
		t.Fatalf("内容变化应原地更新，id 期望 %d，得到 %d", id1, id3)
	}

	got, err := repo.GetDoc(ctx, retrieval.KindTool, "read_file")
	if err != nil {
		t.Fatalf("GetDoc 失败: %v", err)
	}
	if got == nil || got.Body != d.Body {
		t.Fatalf("正文应已更新，得到 %+v", got)
	}

	// 不同 kind 同 key 必须是两条独立记录。
	if _, _, err := repo.UpsertDoc(ctx, retrieval.Doc{
		Kind: retrieval.KindMemoryL2, Key: "read_file", Title: "记忆", Body: "关于 read_file 的笔记",
	}); err != nil {
		t.Fatalf("跨类别同 key 写入失败: %v", err)
	}
	n, err := repo.Count(ctx, retrieval.KindTool)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("工具类别应只有 1 条，得到 %d", n)
	}
}

// FTS5 关键词召回：中文 3 字以上可命中（trigram），且按 bm25 升序。
func TestSearchKeywordFTS5(t *testing.T) {
	ctx := context.Background()
	_, repo := newRetrievalStore(t)

	docs := []retrieval.Doc{
		{Kind: retrieval.KindTool, Key: "read_file", Title: "read_file", Body: "读取工作区文件的工具"},
		{Kind: retrieval.KindTool, Key: "grep", Title: "grep", Body: "在工作区按正则搜索文件内容"},
		{Kind: retrieval.KindTool, Key: "todo", Title: "todo", Body: "任务清单管理"},
	}
	for _, d := range docs {
		if _, _, err := repo.UpsertDoc(ctx, d); err != nil {
			t.Fatalf("写入 %s 失败: %v", d.Key, err)
		}
	}

	// 「工作区」是 3 字，trigram 可命中 read_file 与 grep 两条。
	hits, err := repo.SearchKeyword(ctx, retrieval.KindTool, "工作区", 10)
	if err != nil {
		t.Fatalf("FTS5 检索失败: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("「工作区」期望命中 2 条，得到 %d：%+v", len(hits), hits)
	}
	// 名次必须从 1 起连续（RRF 依赖该约定）。
	for i, h := range hits {
		if h.Rank != i+1 {
			t.Fatalf("第 %d 条名次期望 %d，得到 %d", i, i+1, h.Rank)
		}
	}
	// bm25 升序：SQLite 返回负值，越负越相关。
	if hits[0].BM25 > hits[1].BM25 {
		t.Fatalf("bm25 应升序排列，得到 %v > %v", hits[0].BM25, hits[1].BM25)
	}

	// 英文子串同样可命中。
	if hits, err := repo.SearchKeyword(ctx, retrieval.KindTool, "read", 10); err != nil || len(hits) != 1 {
		t.Fatalf("「read」期望命中 1 条，得到 %d（err=%v）", len(hits), err)
	}

	// 类别隔离：工具检索不得返回记忆类别文档。
	if hits, err := repo.SearchKeyword(ctx, retrieval.KindMemoryL2, "工作区", 10); err != nil || len(hits) != 0 {
		t.Fatalf("记忆类别不应命中工具文档，得到 %d（err=%v）", len(hits), err)
	}
}

// 短查询（<3 字符）走 LIKE 兜底：trigram 索引对 2 字中文无命中。
func TestSearchKeywordShortQueryFallsBackToLike(t *testing.T) {
	ctx := context.Background()
	_, repo := newRetrievalStore(t)

	if _, _, err := repo.UpsertDoc(ctx, retrieval.Doc{
		Kind: retrieval.KindTool, Key: "read_file", Title: "读文件", Body: "读取工作区文件的工具",
	}); err != nil {
		t.Fatal(err)
	}

	// 「工具」只有 2 字：FTS5 trigram 必然 0 命中，必须由 LIKE 兜底捞到。
	hits, err := repo.SearchKeyword(ctx, retrieval.KindTool, "工具", 10)
	if err != nil {
		t.Fatalf("LIKE 兜底检索失败: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("2 字中文应由 LIKE 兜底命中 1 条，得到 %d", len(hits))
	}
	if hits[0].Key != "read_file" {
		t.Fatalf("命中文档错误：%s", hits[0].Key)
	}

	// LIKE 通配符必须被转义：查询 "%" 不得匹配全部文档。
	if hits, err := repo.SearchKeyword(ctx, retrieval.KindTool, "%%", 10); err != nil || len(hits) != 0 {
		t.Fatalf("通配符应被转义，得到 %d 条（err=%v）", len(hits), err)
	}
}

// FTS5 语法字符不得导致报错（统一短语包裹）。
func TestSearchKeywordSyntaxCharsSafe(t *testing.T) {
	ctx := context.Background()
	_, repo := newRetrievalStore(t)

	if _, _, err := repo.UpsertDoc(ctx, retrieval.Doc{
		Kind: retrieval.KindTool, Key: "grep", Title: "grep", Body: `正则 "pattern" 搜索 -x * 内容`,
	}); err != nil {
		t.Fatal(err)
	}

	for _, q := range []string{`"pattern"`, `-x`, `*`, `AND`, `a:b`, `(`, `"`} {
		if _, err := repo.SearchKeyword(ctx, retrieval.KindTool, q, 5); err != nil {
			t.Fatalf("查询 %q 不应报错：%v", q, err)
		}
	}
}

// 向量通道：暴力余弦结果与纯 Go 基线一致（容差 1e-5），且按相似度降序。
func TestSearchVectorBruteForce(t *testing.T) {
	ctx := context.Background()
	_, repo := newRetrievalStore(t)

	type item struct {
		key string
		vec []float32
	}
	items := []item{
		{"a", []float32{1, 0, 0}},
		{"b", []float32{0.9, 0.1, 0}},
		{"c", []float32{0, 1, 0}},
		{"d", []float32{-1, 0, 0}},
	}
	for _, it := range items {
		id, _, err := repo.UpsertDoc(ctx, retrieval.Doc{
			Kind: retrieval.KindMemoryL2, Key: it.key, Title: it.key, Body: "内容 " + it.key,
		})
		if err != nil {
			t.Fatalf("写入 %s 失败: %v", it.key, err)
		}
		if err := repo.PutVector(ctx, id, "fake-3d", it.vec); err != nil {
			t.Fatalf("写入向量 %s 失败: %v", it.key, err)
		}
	}

	query := []float32{1, 0, 0}
	hits, err := repo.SearchVector(ctx, retrieval.KindMemoryL2, query, "fake-3d", 10)
	if err != nil {
		t.Fatalf("向量检索失败: %v", err)
	}
	if len(hits) != 4 {
		t.Fatalf("期望 4 条向量命中，得到 %d", len(hits))
	}

	// 与纯 Go 基线逐条比对（retrieval.Cosine 是同一算法，此处验证 store 侧无偏差）。
	byKey := map[string]float64{}
	for _, h := range hits {
		byKey[h.Key] = h.Score
	}
	for _, it := range items {
		want := retrieval.Cosine(query, it.vec)
		got := byKey[it.key]
		if math.Abs(got-want) > 1e-5 {
			t.Fatalf("%s 余弦期望 %.8f，得到 %.8f", it.key, want, got)
		}
	}

	// 降序：a(1.0) > b > c(0) > d(-1)。
	wantOrder := []string{"a", "b", "c", "d"}
	for i, k := range wantOrder {
		if hits[i].Key != k {
			t.Fatalf("第 %d 位期望 %s，得到 %s（完整：%+v）", i, k, hits[i].Key, hits)
		}
	}

	// 模型隔离：不同 model_id 的向量不参与召回。
	if hits, err := repo.SearchVector(ctx, retrieval.KindMemoryL2, query, "other-model", 10); err != nil || len(hits) != 0 {
		t.Fatalf("异模型应 0 命中，得到 %d（err=%v）", len(hits), err)
	}
}

// 维度不一致的向量必须跳过（换模型未重建时的保护，禁止 NaN 污染排序）。
func TestSearchVectorSkipsDimMismatch(t *testing.T) {
	ctx := context.Background()
	_, repo := newRetrievalStore(t)

	id3, _, err := repo.UpsertDoc(ctx, retrieval.Doc{
		Kind: retrieval.KindMemoryL2, Key: "v3", Title: "v3", Body: "三维向量",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.PutVector(ctx, id3, "m", []float32{1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	id5, _, err := repo.UpsertDoc(ctx, retrieval.Doc{
		Kind: retrieval.KindMemoryL2, Key: "v5", Title: "v5", Body: "五维向量",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.PutVector(ctx, id5, "m", []float32{1, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}

	// 用 3 维查询：只能召回 v3，v5 因维度不符被跳过。
	hits, err := repo.SearchVector(ctx, retrieval.KindMemoryL2, []float32{1, 0, 0}, "", 10)
	if err != nil {
		t.Fatalf("向量检索失败: %v", err)
	}
	if len(hits) != 1 || hits[0].Key != "v3" {
		t.Fatalf("应只召回 3 维向量 v3，得到 %+v", hits)
	}

	// 空查询向量：返回空而非报错。
	if hits, err := repo.SearchVector(ctx, retrieval.KindMemoryL2, nil, "", 10); err != nil || len(hits) != 0 {
		t.Fatalf("空查询应返回空，得到 %d（err=%v）", len(hits), err)
	}
}

// 删除文档后 FTS5 同步失效，且孤立向量被清理。
func TestDeleteDocAndOrphanVectors(t *testing.T) {
	ctx := context.Background()
	m, repo := newRetrievalStore(t)

	id, _, err := repo.UpsertDoc(ctx, retrieval.Doc{
		Kind: retrieval.KindTool, Key: "read_file", Title: "读文件", Body: "读取工作区文件的工具",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.PutVector(ctx, id, "m", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}

	if err := repo.DeleteDoc(ctx, retrieval.KindTool, "read_file"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	// FTS5 触发器应同步删除索引条目。
	if hits, err := repo.SearchKeyword(ctx, retrieval.KindTool, "工作区", 10); err != nil || len(hits) != 0 {
		t.Fatalf("删除后 FTS5 应无命中，得到 %d（err=%v）", len(hits), err)
	}
	// 孤立向量应被清理。
	var vn int
	if err := m.conn().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM retrieval_vectors WHERE doc_id = ?`, id).Scan(&vn); err != nil {
		t.Fatal(err)
	}
	if vn != 0 {
		t.Fatalf("孤立向量应被清理，仍剩 %d 条", vn)
	}
	// 重复删除不报错（幂等）。
	if err := repo.DeleteDoc(ctx, retrieval.KindTool, "read_file"); err != nil {
		t.Fatalf("重复删除应幂等，得到 %v", err)
	}
}

// ListKeys 用于「文件为唯一真源、索引可重建」的差集清理。
func TestListKeysAndIndexMeta(t *testing.T) {
	ctx := context.Background()
	_, repo := newRetrievalStore(t)

	for _, k := range []string{"b_tool", "a_tool", "c_tool"} {
		if _, _, err := repo.UpsertDoc(ctx, retrieval.Doc{
			Kind: retrieval.KindMemoryL3, Key: k, Title: k, Body: "正文",
		}); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := repo.ListKeys(ctx, retrieval.KindMemoryL3)
	if err != nil {
		t.Fatalf("ListKeys 失败: %v", err)
	}
	if len(keys) != 3 || keys[0] != "a_tool" || keys[1] != "b_tool" || keys[2] != "c_tool" {
		t.Fatalf("ListKeys 应升序返回 3 个键，得到 %+v", keys)
	}

	// 索引元信息：不存在返回空串，写入后可读回（幂等覆盖）。
	if v, err := repo.GetIndexMeta(ctx, "embedding"); err != nil || v != "" {
		t.Fatalf("未写入时应返回空串，得到 %q（err=%v）", v, err)
	}
	if err := repo.PutIndexMeta(ctx, "embedding", `{"model":"fake","dim":3}`); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutIndexMeta(ctx, "embedding", `{"model":"fake","dim":4}`); err != nil {
		t.Fatal(err)
	}
	if v, err := repo.GetIndexMeta(ctx, "embedding"); err != nil || v != `{"model":"fake","dim":4}` {
		t.Fatalf("元信息应被覆盖为最新值，得到 %q（err=%v）", v, err)
	}
}

// TouchRef 刷新召回时间（参与记忆时间衰减打分）。
func TestTouchRef(t *testing.T) {
	ctx := context.Background()
	_, repo := newRetrievalStore(t)

	id, _, err := repo.UpsertDoc(ctx, retrieval.Doc{
		Kind: retrieval.KindMemoryL2, Key: "note", Title: "笔记", Body: "内容",
	})
	if err != nil {
		t.Fatal(err)
	}
	const at = int64(123456789)
	if err := repo.TouchRef(ctx, id, at); err != nil {
		t.Fatalf("TouchRef 失败: %v", err)
	}
	got, err := repo.GetDoc(ctx, retrieval.KindMemoryL2, "note")
	if err != nil || got == nil {
		t.Fatalf("GetDoc 失败: %v", err)
	}
	if got.LastRefAt != at {
		t.Fatalf("LastRefAt 期望 %d，得到 %d", at, got.LastRefAt)
	}
}

// 非法文档必须被拒绝（空 Key / 未知 Kind），不得静默入库。
func TestUpsertDocRejectsInvalid(t *testing.T) {
	ctx := context.Background()
	_, repo := newRetrievalStore(t)

	if _, _, err := repo.UpsertDoc(ctx, retrieval.Doc{Kind: retrieval.KindTool, Key: "  "}); err == nil {
		t.Fatal("空 Key 应被拒绝")
	}
	if _, _, err := repo.UpsertDoc(ctx, retrieval.Doc{Kind: retrieval.Kind("bogus"), Key: "k"}); err == nil {
		t.Fatal("未知 Kind 应被拒绝")
	}
}
