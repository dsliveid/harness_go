package retrieval

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Doc 索引文档（与 retrieval_docs 表一一对应）。
//
// 铁律：Body 只存可进 FTS5 的纯文本；二进制/大对象一律走 blob CAS，
// 索引里只留「path:line 引用 + 摘要」，避免索引表膨胀成第二个数据库。
type Doc struct {
	ID          int64
	Kind        Kind
	Key         string  // 唯一键：工具名 / 记忆文件相对路径
	Title       string  // 一行标题（进 FTS5，也用于索引展示）
	Body        string  // 全文（进 FTS5）
	ContentHash string  // sha256，幂等判据
	Importance  float64 // 0~1，参与记忆打分（0.15 权重）
	UpdatedAt   int64   // Unix 毫秒
	LastRefAt   int64   // Unix 毫秒
}

// HashContent 计算索引内容摘要（幂等判据）。
//
// 参与哈希的字段：Kind + Key + Title + Body。
// 刻意不含 Importance/UpdatedAt/LastRefAt —— 这三者是运行时元数据，
// 它们变化不应触发「内容已变更」的重新索引（否则每次召回都改 last_ref_at 就全量重写）。
func HashContent(kind Kind, key, title, body string) string {
	h := sha256.New()
	h.Write([]byte(string(kind)))
	h.Write([]byte{0})
	h.Write([]byte(key))
	h.Write([]byte{0})
	h.Write([]byte(title))
	h.Write([]byte{0})
	h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil))
}

// Hash 返回该文档的内容摘要。
func (d Doc) Hash() string { return HashContent(d.Kind, d.Key, d.Title, d.Body) }

// Normalize 清理并校验文档，返回可直接落库的副本。
//
// 清理规则：
//   - Key 去首尾空白；空 Key 视为非法（返回 false）；
//   - Kind 非法视为非法；
//   - Title 折叠换行并截断到 200 字符（标题只做索引展示，不进正文）；
//   - Body 折叠连续空行为单空行（减少 FTS5 索引体积，不影响召回）；
//   - Importance 钳制到 [0,1]，零值回落 0.5（中性）。
func (d Doc) Normalize() (Doc, bool) {
	out := d
	out.Key = strings.TrimSpace(d.Key)
	if out.Key == "" || !d.Kind.Valid() {
		return Doc{}, false
	}
	out.Title = oneLine(d.Title, 200)
	out.Body = collapseBlank(d.Body)
	if out.Importance < 0 {
		out.Importance = 0
	} else if out.Importance > 1 {
		out.Importance = 1
	} else if out.Importance == 0 {
		out.Importance = 0.5
	}
	out.ContentHash = out.Hash()
	return out, true
}

// oneLine 折叠所有空白为单空格并截断（按 rune 截断，避免切坏 UTF-8）。
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// collapseBlank 把 2 个以上连续换行压成 1 个（保留段落分隔语义）。
func collapseBlank(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}

// DocRepo 索引仓储端口（由 internal/store 实现，主库 retrieval_docs）。
//
// 铁律：本接口只暴露索引域操作，不暴露 *sql.DB；store 侧禁止把连接传给 retrieval 包。
type DocRepo interface {
	// UpsertDoc 幂等写入：同 (Kind,Key) 且 ContentHash 相同则只刷新 LastRefAt，不重写正文。
	// 返回 (docID, changed, err)，changed=false 表示命中幂等未发生写入。
	UpsertDoc(doc Doc) (int64, bool, error)
	// GetDoc 按 (Kind,Key) 取文档；不存在返回 (nil, nil)。
	GetDoc(kind Kind, key string) (*Doc, error)
	// DeleteDoc 删除文档（FTS5 由触发器同步）；不存在不算错误。
	DeleteDoc(kind Kind, key string) error
	// ListKeys 列出某类别的全部 Key（用于「文件为真源、索引可重建」的差集清理）。
	ListKeys(kind Kind) ([]string, error)
	// TouchRef 刷新 LastRefAt（召回后调用，参与记忆时间衰减打分）。
	TouchRef(docID int64, at int64) error
	// Count 返回某类别文档数（启动自检与预算端点用）。
	Count(kind Kind) (int, error)
}
