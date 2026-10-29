package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrBlobTooLarge 单 blob 超过配置上限。
var ErrBlobTooLarge = errors.New("store: blob 超过大小上限")

// BlobRef 一次 blob 写入的结果。
type BlobRef struct {
	Hash string
	Size int64
	Mime string
}

// BlobMeta blob 元信息。
type BlobMeta struct {
	Hash string
	Size int64
}

// SessionBlobs 会话级 CAS（内容寻址，会话内去重）。
//
// 写入铁律（docs/数据存储方案.md §3）：
//  1. 流式写入 data/tmp/<uuid>.part，同时计算 sha256
//  2. fsync
//  3. 目标 sessions/<sid>/blobs/<xx>/<hash>：已存在则复用（去重命中），否则同盘 rename
//  4. 会话库同事务 INSERT OR IGNORE blobs —— 由调用方完成（本类型只管文件）
//
// 顺序不可颠倒：先文件后库。反了会出现"库有记录、文件缺失 → 读取 500"。
type SessionBlobs struct {
	root     string // sessions/<sid>/blobs
	tmpDir   string // data/tmp
	maxBytes int64
}

// NewSessionBlobs 创建会话级 CAS，并校验 tmp 与 blobs 同盘（跨盘 rename 非原子）。
func NewSessionBlobs(blobRoot, tmpDir string, maxBytes int64) (*SessionBlobs, error) {
	if blobRoot == "" || tmpDir == "" {
		return nil, errors.New("store: blobRoot 与 tmpDir 不能为空")
	}
	if maxBytes <= 0 {
		maxBytes = 64 << 20 // 默认 64MB
	}
	if err := os.MkdirAll(blobRoot, 0o755); err != nil {
		return nil, fmt.Errorf("store: 创建 blob 目录失败: %w", err)
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("store: 创建 tmp 目录失败: %w", err)
	}
	if !sameVolume(blobRoot, tmpDir) {
		return nil, fmt.Errorf("store: tmp(%s) 与 blob(%s) 必须位于同一磁盘，跨盘 rename 非原子", tmpDir, blobRoot)
	}
	return &SessionBlobs{root: blobRoot, tmpDir: tmpDir, maxBytes: maxBytes}, nil
}

// sameVolume 判断两目录是否同盘。Windows 比较卷名；Unix 无卷概念，视为同盘。
func sameVolume(a, b string) bool {
	return strings.EqualFold(filepath.VolumeName(a), filepath.VolumeName(b))
}

// MaxBytes 返回单 blob 上限（字节）。
func (b *SessionBlobs) MaxBytes() int64 { return b.maxBytes }

// Path 返回 blob 的落盘路径（<root>/<前两位>/<hash>）。
func (b *SessionBlobs) Path(hash string) string {
	prefix := "xx"
	if len(hash) >= 2 {
		prefix = hash[:2]
	}
	return filepath.Join(b.root, prefix, hash)
}

// Put 流式写入 blob，返回内容寻址结果。
func (b *SessionBlobs) Put(ctx context.Context, r io.Reader, mime string) (BlobRef, error) {
	part := filepath.Join(b.tmpDir, newUUID()+".part")
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return BlobRef{}, fmt.Errorf("store: 创建临时文件失败: %w", err)
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(part)
	}

	h := sha256.New()
	// 多读 1 字节用于判断是否超限
	written, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, b.maxBytes+1))
	if err != nil {
		cleanup()
		return BlobRef{}, fmt.Errorf("store: 写入 blob 失败: %w", err)
	}
	if written > b.maxBytes {
		cleanup()
		return BlobRef{}, fmt.Errorf("%w: %d > %d 字节", ErrBlobTooLarge, written, b.maxBytes)
	}
	if err := ctx.Err(); err != nil {
		cleanup()
		return BlobRef{}, err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return BlobRef{}, fmt.Errorf("store: fsync 失败: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(part)
		return BlobRef{}, fmt.Errorf("store: 关闭临时文件失败: %w", err)
	}

	hash := hex.EncodeToString(h.Sum(nil))
	ref := BlobRef{Hash: hash, Size: written, Mime: mime}
	dst := b.Path(hash)

	// 去重命中：同内容已存在，丢弃临时文件
	if _, statErr := os.Stat(dst); statErr == nil {
		_ = os.Remove(part)
		return ref, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		_ = os.Remove(part)
		return BlobRef{}, fmt.Errorf("store: 创建 blob 分桶目录失败: %w", err)
	}
	if err := os.Rename(part, dst); err != nil {
		// Windows 上目标已存在会失败；并发写入同内容时按去重处理
		if _, statErr := os.Stat(dst); statErr == nil {
			_ = os.Remove(part)
			return ref, nil
		}
		_ = os.Remove(part)
		return BlobRef{}, fmt.Errorf("store: rename blob 失败: %w", err)
	}
	return ref, nil
}

// Open 打开 blob 读取流。
func (b *SessionBlobs) Open(_ context.Context, hash string) (io.ReadCloser, error) {
	f, err := os.Open(b.Path(hash))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: blob %s", ErrNotFound, hash)
		}
		return nil, fmt.Errorf("store: 打开 blob 失败: %w", err)
	}
	return f, nil
}

// Stat 返回 blob 元信息。
func (b *SessionBlobs) Stat(_ context.Context, hash string) (BlobMeta, error) {
	fi, err := os.Stat(b.Path(hash))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return BlobMeta{}, fmt.Errorf("%w: blob %s", ErrNotFound, hash)
		}
		return BlobMeta{}, fmt.Errorf("store: 读取 blob 元信息失败: %w", err)
	}
	return BlobMeta{Hash: hash, Size: fi.Size()}, nil
}

// RecordBlob 在会话库登记 blob（与业务写入同事务调用）。
func RecordBlob(ctx context.Context, tx *sql.Tx, ref BlobRef) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO blobs(hash, size, mime, created_at) VALUES(?,?,?,?)
		 ON CONFLICT(hash) DO NOTHING`,
		ref.Hash, ref.Size, ref.Mime, NowMillis())
	if err != nil {
		return fmt.Errorf("store: 登记 blob 失败: %w", err)
	}
	return nil
}

// CleanupTmp 清理残留的 .part 文件（启动时调用，防半写残留）。
func CleanupTmp(tmpDir string) (int, error) {
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: 扫描 tmp 目录失败: %w", err)
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
	return removed, nil
}

// newUUID 生成 16 字节随机十六进制串（临时文件名用，无需标准 UUID 格式）。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "fallback-" + hex.EncodeToString([]byte(fmt.Sprintf("%d", NowMillis())))
	}
	return hex.EncodeToString(b[:])
}
