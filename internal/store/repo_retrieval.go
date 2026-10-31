package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"harness-go/internal/retrieval"
)

// ============ 检索索引仓储（主库，阶段 D / Token 治理 M6） ============
//
// 归属：全部在主库。工具索引与记忆 L2/L3 跨会话共享，放主库天然避免跨库事务
// （docs/数据存储方案.md v2.0 §4「零跨库事务」）。
//
// 两条召回通道：
//   - 关键词：FTS5 MATCH + bm25()（external content 模式，正文不重复存储）
//   - 向量：retrieval_vectors 读出候选集后在 Go 内存暴力余弦
//     （sqlite-vec 需 CGO，违反「纯 Go 无 CGO 单二进制」铁律）

// RetrievalRepo 检索索引仓储（主库）。
type RetrievalRepo struct{ s *MainStore }

// Retrieval 返回检索仓储。
func (s *MainStore) Retrieval() *RetrievalRepo { return &RetrievalRepo{s: s} }

const docCols = `id, kind, key, title, body, content_hash, importance, updated_at, last_ref_at`

func scanDoc(sc interface{ Scan(...any) error }) (retrieval.Doc, error) {
	var d retrieval.Doc
	var kind string
	err := sc.Scan(&d.ID, &kind, &d.Key, &d.Title, &d.Body, &d.ContentHash,
		&d.Importance, &d.UpdatedAt, &d.LastRefAt)
	d.Kind = retrieval.Kind(kind)
	return d, err
}

// UpsertDoc 幂等写入索引文档。
//
// 幂等语义（Token 治理的关键：避免每轮对话都全量重写索引）：
//   - 同 (kind,key) 且 content_hash 相同 → 只刷新 last_ref_at，返回 changed=false；
//   - 内容变化 → 原地 UPDATE（FTS5 由 AFTER UPDATE 触发器同步）；
//   - 不存在 → INSERT。
//
// 单库事务内完成，零跨库。
func (r *RetrievalRepo) UpsertDoc(ctx context.Context, doc retrieval.Doc) (int64, bool, error) {
	norm, ok := doc.Normalize()
	if !ok {
		return 0, false, fmt.Errorf("store: 索引文档非法（kind=%q key=%q）", doc.Kind, doc.Key)
	}
	now := NowMillis()
	if norm.UpdatedAt == 0 {
		norm.UpdatedAt = now
	}
	if norm.LastRefAt == 0 {
		norm.LastRefAt = now
	}

	var (
		id      int64
		changed bool
	)
	err := r.s.withTx(ctx, func(tx *sql.Tx) error {
		var oldID int64
		var oldHash string
		err := tx.QueryRowContext(ctx,
			`SELECT id, content_hash FROM retrieval_docs WHERE kind = ? AND key = ?`,
			string(norm.Kind), norm.Key).Scan(&oldID, &oldHash)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			res, ierr := tx.ExecContext(ctx,
				`INSERT INTO retrieval_docs
				   (kind, key, title, body, content_hash, importance, updated_at, last_ref_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				string(norm.Kind), norm.Key, norm.Title, norm.Body, norm.ContentHash,
				norm.Importance, norm.UpdatedAt, norm.LastRefAt)
			if ierr != nil {
				return fmt.Errorf("store: 插入索引文档失败: %w", ierr)
			}
			id, _ = res.LastInsertId()
			changed = true
			return nil
		case err != nil:
			return fmt.Errorf("store: 查询索引文档失败: %w", err)
		}

		id = oldID
		if oldHash == norm.ContentHash {
			// 幂等命中：只刷新召回时间，正文与哈希不动（不触发 FTS5 重建索引）。
			if _, uerr := tx.ExecContext(ctx,
				`UPDATE retrieval_docs SET last_ref_at = ? WHERE id = ?`, norm.LastRefAt, id); uerr != nil {
				return fmt.Errorf("store: 刷新索引文档时间失败: %w", uerr)
			}
			return nil
		}
		if _, uerr := tx.ExecContext(ctx,
			`UPDATE retrieval_docs
			    SET title = ?, body = ?, content_hash = ?, importance = ?, updated_at = ?, last_ref_at = ?
			  WHERE id = ?`,
			norm.Title, norm.Body, norm.ContentHash, norm.Importance, norm.UpdatedAt, norm.LastRefAt, id); uerr != nil {
			return fmt.Errorf("store: 更新索引文档失败: %w", uerr)
		}
		changed = true
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return id, changed, nil
}

// GetDoc 按 (kind,key) 取索引文档；不存在返回 (nil, nil)。
func (r *RetrievalRepo) GetDoc(ctx context.Context, kind retrieval.Kind, key string) (*retrieval.Doc, error) {
	row := r.s.conn().QueryRowContext(ctx,
		`SELECT `+docCols+` FROM retrieval_docs WHERE kind = ? AND key = ?`, string(kind), key)
	d, err := scanDoc(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取索引文档失败: %w", err)
	}
	return &d, nil
}

// DeleteDoc 删除索引文档（FTS5 由 AFTER DELETE 触发器同步）。
func (r *RetrievalRepo) DeleteDoc(ctx context.Context, kind retrieval.Kind, key string) error {
	return r.s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM retrieval_docs WHERE kind = ? AND key = ?`, string(kind), key); err != nil {
			return fmt.Errorf("store: 删除索引文档失败: %w", err)
		}
		// 向量表无触发器（SQLite 外键 ON DELETE CASCADE 在 WAL 下可靠，但显式删更稳）。
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM retrieval_vectors
			  WHERE doc_id NOT IN (SELECT id FROM retrieval_docs)`); err != nil {
			return fmt.Errorf("store: 清理孤立向量失败: %w", err)
		}
		return nil
	})
}

// ListKeys 列出某类别全部 Key（升序），用于「文件为唯一真源、索引可重建」的差集清理。
func (r *RetrievalRepo) ListKeys(ctx context.Context, kind retrieval.Kind) ([]string, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT key FROM retrieval_docs WHERE kind = ? ORDER BY key ASC`, string(kind))
	if err != nil {
		return nil, fmt.Errorf("store: 列出索引键失败: %w", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("store: 扫描索引键失败: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历索引键失败: %w", err)
	}
	return keys, nil
}

// TouchRef 刷新最后召回时间（参与记忆时间衰减打分）。
func (r *RetrievalRepo) TouchRef(ctx context.Context, docID int64, at int64) error {
	if _, err := r.s.conn().ExecContext(ctx,
		`UPDATE retrieval_docs SET last_ref_at = ? WHERE id = ?`, at, docID); err != nil {
		return fmt.Errorf("store: 刷新召回时间失败: %w", err)
	}
	return nil
}

// Count 返回某类别文档数。
func (r *RetrievalRepo) Count(ctx context.Context, kind retrieval.Kind) (int, error) {
	var n int
	if err := r.s.conn().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM retrieval_docs WHERE kind = ?`, string(kind)).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: 统计索引文档失败: %w", err)
	}
	return n, nil
}

// ============ 关键词通道（FTS5 + bm25） ============

// SearchKeyword 用 FTS5 MATCH 检索指定类别，按 bm25 升序（最相关在前）。
//
// 三个必须处理的边界：
//  1. 查询串短于 3 字符：trigram 分词器无命中（三字符滑窗），改用 LIKE 兜底扫描
//     （<5k 条时开销可忽略，且只扫目标类别）。
//  2. FTS5 语法字符（引号、*、-、:、括号等）：直接拼进 MATCH 会报错，
//     统一用双引号包裹为短语查询，内部双引号转义为两个双引号。
//  3. 无命中返回空切片而非错误（召回为空是正常态）。
func (r *RetrievalRepo) SearchKeyword(ctx context.Context, kind retrieval.Kind, query string, topN int) ([]retrieval.KeywordHit, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	if topN <= 0 {
		topN = 20
	}

	// 短查询（<3 rune）走 LIKE 兜底：trigram 索引建不出来。
	if len([]rune(q)) < 3 {
		return r.searchLike(ctx, kind, q, topN)
	}

	// 短语查询：整串作为一个 phrase，规避 FTS5 运算符注入与语法错误。
	phrase := `"` + strings.ReplaceAll(q, `"`, `""`) + `"`
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT d.id, d.key, bm25(retrieval_fts) AS score
		   FROM retrieval_fts f
		   JOIN retrieval_docs d ON d.id = f.rowid
		  WHERE retrieval_fts MATCH ? AND d.kind = ?
		  ORDER BY score ASC
		  LIMIT ?`, phrase, string(kind), topN)
	if err != nil {
		return nil, fmt.Errorf("store: FTS5 检索失败: %w", err)
	}
	defer rows.Close()

	var hits []retrieval.KeywordHit
	rank := 0
	for rows.Next() {
		var h retrieval.KeywordHit
		if err := rows.Scan(&h.DocID, &h.Key, &h.BM25); err != nil {
			return nil, fmt.Errorf("store: 扫描 FTS5 结果失败: %w", err)
		}
		rank++
		h.Rank = rank
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 FTS5 结果失败: %w", err)
	}
	return hits, nil
}

// searchLike 短查询兜底：在 title/body 上做子串匹配。
//
// 打分约定：与 FTS5 的 bm25 保持一致的方向（越小越相关），
// 用「负的相关度」模拟：标题命中权重高于正文命中。
func (r *RetrievalRepo) searchLike(ctx context.Context, kind retrieval.Kind, q string, topN int) ([]retrieval.KeywordHit, error) {
	pat := "%" + escapeLike(q) + "%"
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT id, key,
		        CASE WHEN title LIKE ? ESCAPE '\' THEN -2.0 ELSE -1.0 END AS score
		   FROM retrieval_docs
		  WHERE kind = ? AND (title LIKE ? ESCAPE '\' OR body LIKE ? ESCAPE '\')
		  ORDER BY score ASC, updated_at DESC
		  LIMIT ?`, pat, string(kind), pat, pat, topN)
	if err != nil {
		return nil, fmt.Errorf("store: LIKE 检索失败: %w", err)
	}
	defer rows.Close()

	var hits []retrieval.KeywordHit
	rank := 0
	for rows.Next() {
		var h retrieval.KeywordHit
		if err := rows.Scan(&h.DocID, &h.Key, &h.BM25); err != nil {
			return nil, fmt.Errorf("store: 扫描 LIKE 结果失败: %w", err)
		}
		rank++
		h.Rank = rank
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 LIKE 结果失败: %w", err)
	}
	return hits, nil
}

// escapeLike 转义 LIKE 通配符（配合 ESCAPE '\'）。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// ============ 向量通道（float32 BLOB + 内存暴力余弦） ============

// PutVector 写入/覆盖文档向量（同 doc_id 覆盖；模型变更需先清空再重建）。
func (r *RetrievalRepo) PutVector(ctx context.Context, docID int64, modelID string, vec []float32) error {
	blob, err := retrieval.EncodeVector(vec)
	if err != nil {
		return fmt.Errorf("store: 编码向量失败: %w", err)
	}
	if _, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO retrieval_vectors (doc_id, model_id, dim, vec, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(doc_id) DO UPDATE SET
		   model_id = excluded.model_id, dim = excluded.dim,
		   vec = excluded.vec, updated_at = excluded.updated_at`,
		docID, modelID, len(vec), blob, NowMillis()); err != nil {
		return fmt.Errorf("store: 写入向量失败: %w", err)
	}
	return nil
}

// SearchVector 读取候选集后在内存中暴力余弦，返回 topN。
//
// 为什么暴力扫描：sqlite-vec 需 CGO（load_extension），违反纯 Go 铁律；
// 工具与记忆总量 <5k 条时，单次全量余弦 <5ms，远低于一次 LLM 往返，
// 文档 §向量检索选型已明确认可该折中。
//
// modelID 非空时只在该模型的向量中检索（换模型后旧向量自然被排除，避免维度错配）。
func (r *RetrievalRepo) SearchVector(ctx context.Context, kind retrieval.Kind, query []float32, modelID string, topN int) ([]retrieval.VectorHit, error) {
	if len(query) == 0 {
		return nil, nil
	}
	if topN <= 0 {
		topN = 20
	}
	q := make([]float64, len(query))
	for i, f := range query {
		q[i] = float64(f)
	}
	// 预计算查询向量模长，避免候选循环里重复开方。
	var qn float64
	for _, x := range q {
		qn += x * x
	}
	if qn == 0 {
		return nil, nil
	}
	qNorm := math.Sqrt(qn)

	sqlStr := `SELECT v.doc_id, d.key, v.model_id, v.dim, v.vec
	             FROM retrieval_vectors v
	             JOIN retrieval_docs d ON d.id = v.doc_id
	            WHERE d.kind = ?`
	args := []any{string(kind)}
	if modelID != "" {
		sqlStr += ` AND v.model_id = ?`
		args = append(args, modelID)
	}

	rows, err := r.s.conn().QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 读取向量失败: %w", err)
	}
	defer rows.Close()

	type cand struct {
		hit  retrieval.VectorHit
		blob []byte
	}
	var cands []cand
	for rows.Next() {
		var (
			docID int64
			key   string
			mid   string
			dim   int
			blob  []byte
		)
		if err := rows.Scan(&docID, &key, &mid, &dim, &blob); err != nil {
			return nil, fmt.Errorf("store: 扫描向量失败: %w", err)
		}
		// 维度不一致的向量直接跳过（换模型未重建时的保护，禁止 NaN 污染排序）。
		if dim != len(query) {
			continue
		}
		cands = append(cands, cand{
			hit:  retrieval.VectorHit{DocID: docID, Key: key, ModelID: mid, Dim: dim},
			blob: blob,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历向量失败: %w", err)
	}
	if len(cands) == 0 {
		return nil, nil
	}

	hits := make([]retrieval.VectorHit, 0, len(cands))
	for _, c := range cands {
		vec, derr := retrieval.DecodeVector(c.blob)
		if derr != nil {
			continue // 损坏的单条向量跳过，不影响整体召回
		}
		if len(vec) != len(query) {
			continue
		}
		var dot, cn float64
		for i := range vec {
			x := float64(vec[i])
			dot += q[i] * x
			cn += x * x
		}
		if cn == 0 {
			continue
		}
		c.hit.Score = dot / (qNorm * math.Sqrt(cn))
		hits = append(hits, c.hit)
	}

	// 按余弦降序，同分按 DocID 升序（确定性，便于测试与前缀缓存）。
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].DocID < hits[j].DocID
	})
	if len(hits) > topN {
		hits = hits[:topN]
	}
	return hits, nil
}

// PutIndexMeta 写入索引元信息（单值键，供启动自检与重建判定）。
func (r *RetrievalRepo) PutIndexMeta(ctx context.Context, key, valueJSON string) error {
	if _, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO index_meta (key, value_json, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at`,
		key, valueJSON, NowMillis()); err != nil {
		return fmt.Errorf("store: 写入索引元信息失败: %w", err)
	}
	return nil
}

// GetIndexMeta 读取索引元信息；不存在返回 ("", nil)。
func (r *RetrievalRepo) GetIndexMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := r.s.conn().QueryRowContext(ctx,
		`SELECT value_json FROM index_meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: 读取索引元信息失败: %w", err)
	}
	return v, nil
}
