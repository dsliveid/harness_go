-- 主库 0003：检索基础设施（阶段 D / Token 治理 M6）
-- 依据 docs/开发实施计划.md 阶段 D 与 docs/多端协同架构可行性方案.md §向量检索选型。
--
-- 设计要点（三条实测结论决定）：
--   1. FTS5 可用：modernc.org/sqlite v1.61.0（SQLite 3.54.1，纯 Go 无 CGO）实测支持
--      CREATE VIRTUAL TABLE ... USING fts5 → 关键词召回用 FTS5 + bm25()，零额外依赖。
--   2. sqlite-vec 不可用：它是 C 扩展，需 load_extension，modernc 驱动不支持（要 CGO，
--      违反项目「纯 Go 无 CGO 单二进制」铁律）→ 向量改存 float32 小端 BLOB，
--      由 Go 在内存中暴力余弦（工具/记忆 <5k 条时 <5ms，文档已认可）。
--   3. 索引表必须带 model_id + dim：换 embedding 模型即后台重建，避免维度错配。
--
-- 归属：全部在主库。工具索引与记忆 L2/L3 跨会话共享，放主库天然避免跨库事务。
-- 铁律：已发布的迁移文件永不修改，只能追加新文件。

-- ============ 索引文档（唯一真源表，FTS5 与向量表都挂在它的 id 上） ============
CREATE TABLE IF NOT EXISTS retrieval_docs (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  kind         TEXT    NOT NULL,                    -- tool | memory_l2 | memory_l3
  key          TEXT    NOT NULL,                    -- 工具名 / 记忆文件相对路径
  title        TEXT    NOT NULL DEFAULT '',         -- 一行标题（进 FTS5，也用于索引展示）
  body         TEXT    NOT NULL DEFAULT '',         -- 全文（进 FTS5）
  content_hash TEXT    NOT NULL DEFAULT '',         -- sha256，幂等判据：同 key 同 hash 不重写
  importance   REAL    NOT NULL DEFAULT 0.5,        -- 0~1，参与记忆打分（0.15 权重）
  updated_at   INTEGER NOT NULL DEFAULT 0,          -- Unix 毫秒（内容最后变更）
  last_ref_at  INTEGER NOT NULL DEFAULT 0,          -- Unix 毫秒（最后被召回，参与时间衰减）
  UNIQUE(kind, key)
);
CREATE INDEX IF NOT EXISTS idx_retrieval_docs_kind ON retrieval_docs(kind, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_retrieval_docs_hash ON retrieval_docs(content_hash);

-- ============ FTS5 关键词索引（external content，正文不重复存储） ============
-- external content 模式下 FTS5 不建 _content 影子表，只建 _data/_idx/_docsize/_config 四张。
-- 用触发器保持与 retrieval_docs 同步，写入方只需操作 retrieval_docs。
-- tokenizer 选型：trigram（SQLite ≥3.34，本机 3.54.1 实测可用）。
-- 理由：本项目语料以中文为主，unicode61 会把「工具索引」整串当成单个 token，
-- 导致搜「工具」无法命中；trigram 按三字符滑窗建索引，天然支持中文/英文子串匹配。
-- 代价：查询串短于 3 字符时 FTS5 无命中 → 由 store 层 LIKE 兜底扫描（<5k 条开销可忽略）。
CREATE VIRTUAL TABLE IF NOT EXISTS retrieval_fts USING fts5(
  title,
  body,
  content='retrieval_docs',
  content_rowid='id',
  tokenize='trigram'
);

CREATE TRIGGER IF NOT EXISTS retrieval_docs_ai AFTER INSERT ON retrieval_docs BEGIN
  INSERT INTO retrieval_fts(rowid, title, body) VALUES (new.id, new.title, new.body);
END;

CREATE TRIGGER IF NOT EXISTS retrieval_docs_ad AFTER DELETE ON retrieval_docs BEGIN
  INSERT INTO retrieval_fts(retrieval_fts, rowid, title, body)
    VALUES ('delete', old.id, old.title, old.body);
END;

CREATE TRIGGER IF NOT EXISTS retrieval_docs_au AFTER UPDATE ON retrieval_docs BEGIN
  INSERT INTO retrieval_fts(retrieval_fts, rowid, title, body)
    VALUES ('delete', old.id, old.title, old.body);
  INSERT INTO retrieval_fts(rowid, title, body) VALUES (new.id, new.title, new.body);
END;

-- ============ 向量表（float32 小端 BLOB，替代 sqlite-vec） ============
CREATE TABLE IF NOT EXISTS retrieval_vectors (
  doc_id     INTEGER PRIMARY KEY,                   -- 1:1 关联 retrieval_docs.id
  model_id   TEXT    NOT NULL DEFAULT '',           -- embedding 模型标识，换模型即重建
  dim        INTEGER NOT NULL DEFAULT 0,            -- 维度（len(vec)/4）
  vec        BLOB    NOT NULL,                      -- float32 小端连续排列
  updated_at INTEGER NOT NULL DEFAULT 0,
  FOREIGN KEY (doc_id) REFERENCES retrieval_docs(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_retrieval_vectors_model ON retrieval_vectors(model_id, dim);

-- ============ 索引元信息（单库单值键，供启动自检与重建判定） ============
CREATE TABLE IF NOT EXISTS index_meta (
  key        TEXT PRIMARY KEY,
  value_json TEXT NOT NULL,
  updated_at INTEGER NOT NULL DEFAULT 0
);
