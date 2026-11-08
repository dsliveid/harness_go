package logvault

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// inlineThreshold 正文内联阈值：≤8KB 直接内联到 payload_inline，>8KB 外置为 blob。
//
// 对齐桌面端 logs.rs:386-395 的 externalize() 思路，但外置目标是
// data/logs/blob/<day>/，**绝不进会话级 CAS**（否则日志会污染会话导出/备份/删除）。
const inlineThreshold = 8 << 10

// previewLimit 预览长度上限（脱敏后的前 2KB 存表内，供界面直接展示与子串搜索）。
const previewLimit = 2 << 10

// BlobStore 天分片正文 CAS（内容寻址，同一天内去重）。
//
// 写入顺序铁律（与 store.SessionBlobs 一致，不可颠倒）：
//  1. 流式写入同盘临时文件，同时计算 sha256
//  2. fsync
//  3. 目标 blob/<day>/<xx>/<hash>：已存在则复用（去重命中），否则同盘 rename
//
// 顺序反了会出现「文件缺失但表里有引用 → 读取 500」。
type BlobStore struct {
	root string // data/logs
}

// NewBlobStore 创建正文 CAS。
func NewBlobStore(shard *Shard) *BlobStore {
	return &BlobStore{root: shard.Root()}
}

// blobPath 返回 blob 绝对路径（<day>/<xx>/<hash>，xx = hash 前 2 位分桶）。
func (b *BlobStore) blobPath(day, hash string) string {
	xx := "00"
	if len(hash) >= 2 {
		xx = hash[:2]
	}
	return filepath.Join(b.root, "blob", day, xx, hash)
}

// Exists 判断 blob 是否已存在。
func (b *BlobStore) Exists(day, hash string) bool {
	_, err := os.Stat(b.blobPath(day, hash))
	return err == nil
}

// Put 写入正文并返回 sha256 与字节数。已存在（去重命中）时直接复用。
func (b *BlobStore) Put(day string, data []byte) (hash string, size int64, err error) {
	sum := sha256.Sum256(data)
	hash = hex.EncodeToString(sum[:])
	size = int64(len(data))

	target := b.blobPath(day, hash)
	if _, statErr := os.Stat(target); statErr == nil {
		return hash, size, nil // 去重命中
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, fmt.Errorf("logvault: 创建正文目录失败: %w", err)
	}
	tmpDir := filepath.Join(b.root, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", 0, fmt.Errorf("logvault: 创建临时目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(tmpDir, "logblob-*.part")
	if err != nil {
		return "", 0, fmt.Errorf("logvault: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // rename 成功后此处为 no-op

	if _, err := io.Copy(tmp, strings.NewReader(string(data))); err != nil {
		_ = tmp.Close()
		return "", 0, fmt.Errorf("logvault: 写入正文失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", 0, fmt.Errorf("logvault: fsync 正文失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("logvault: 关闭正文临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		// 并发写入同一 hash：后到者失败但内容相同，视作成功
		if _, statErr := os.Stat(target); statErr == nil {
			return hash, size, nil
		}
		return "", 0, fmt.Errorf("logvault: 落定正文失败: %w", err)
	}
	return hash, size, nil
}

// Get 读取正文全文（文件不存在返回空串与 nil，交由上层提示「正文已清理」）。
func (b *BlobStore) Get(day, hash string) ([]byte, error) {
	if hash == "" {
		return nil, nil
	}
	data, err := os.ReadFile(b.blobPath(day, hash))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("logvault: 读取正文失败: %w", err)
	}
	return data, nil
}

// CleanupTmp 清理异常退出残留的正文临时文件（启动时调用）。
func (b *BlobStore) CleanupTmp() int {
	tmpDir := filepath.Join(b.root, "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		if err := os.Remove(filepath.Join(tmpDir, e.Name())); err == nil {
			removed++
		}
	}
	return removed
}

// truncatePreview 截断到预览上限（按 rune 边界，避免切碎 UTF-8）。
//
// ⚠️ 性能要点：本函数在**每一行**日志写入时调用，且输入可达数十 KB。
// 旧实现逐 rune 调用 len(string(c))（每次都是一次小字符串分配），在 40KB 输入上
// 会产生数万次分配、数十毫秒开销。此处改为：先做廉价的字节长度判断，
// 只在确需截断时才走 UTF-8 边界回退，且回退用 utf8.RuneStart 判定，零分配。
func truncatePreview(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s // 绝大多数情况：直接返回，零拷贝
	}
	cut := limit
	// 向前回退到合法的 UTF-8 起始字节（最多回退 3 字节，因为 UTF-8 最长 4 字节）
	for back := 0; back < utf8.UTFMax && cut > 0; back++ {
		if utf8.RuneStart(s[cut]) {
			return s[:cut]
		}
		cut--
	}
	if cut <= 0 {
		return ""
	}
	return s[:cut]
}
