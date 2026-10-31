// Package snapshot 实现基础服务 S10「快照与回滚」（阶段 G）。
//
// 组成：
//   - GlobalBlobs：全局文件内容 CAS（data/snapshots/blobs/<xx>/<sha256>），
//     跨会话按内容去重；引用计数在主库 snapshot_blobs（先文件后库铁律同 blob）。
//   - Store：toolapi.SnapshotStore + SnapshotRegistrar 实装——fs 插件写入前/后
//     经 Host.Snapshots() 登记 before/after 锚点快照，长任务检查点登记
//     checkpoint 快照；Restore 是**物理写回**（tmp→rename 原子），不是改标记。
//   - revert & reapply 的编排（消息/轮次/工具级）在 internal/service.RollbackService：
//     本包只提供单快照的登记与恢复原语。
//
// 一致性铁律（docs/基础服务设计.md S10）：
//   - 写入前必须先登记快照（fs 插件强制）——先快照后落盘，回滚才不丢内容；
//   - 恢复是物理写回；回滚点之后的消息置灰但保留（session 库 hidden 列）；
//   - GC 按引用计数：快照行删除 → 计数归零 → 物理文件清理。
package snapshot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrSnapshotTooLarge 单文件超过快照大小上限。
var ErrSnapshotTooLarge = errors.New("snapshot: 内容超过快照大小上限")

// ErrInvalidSnapshot 快照引用无效（ID 未知或 CAS 内容缺失）。
var ErrInvalidSnapshot = errors.New("snapshot: 快照引用无效")

// MaxSnapshotBytes 单文件快照上限（与会话 blob 上限同口径）。
const MaxSnapshotBytes int64 = 64 << 20

// GlobalBlobs 全局快照内容 CAS。
//
//   - Put 流式写 data/tmp/<uuid>.part → fsync → 同盘 rename 到
//     snapshots/blobs/<xx>/<sha256>（已存在即去重命中）；
//   - 引用计数由主库 snapshot_blobs 管理（Store 负责调用仓储）。
type GlobalBlobs struct {
	root   string // data/snapshots/blobs
	tmpDir string // data/tmp（与 root 同盘，跨盘 rename 非原子）
}

// NewGlobalBlobs 创建全局快照 CAS 并校验 tmp 同盘。
func NewGlobalBlobs(dataDir string) (*GlobalBlobs, error) {
	root := filepath.Join(dataDir, "snapshots", "blobs")
	tmpDir := filepath.Join(dataDir, "tmp")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("snapshot: 创建快照 CAS 目录失败: %w", err)
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("snapshot: 创建 tmp 目录失败: %w", err)
	}
	if !sameVolume(root, tmpDir) {
		return nil, fmt.Errorf("snapshot: tmp(%s) 与快照 CAS(%s) 必须同盘", tmpDir, root)
	}
	return &GlobalBlobs{root: root, tmpDir: tmpDir}, nil
}

// sameVolume 判断两目录是否同盘（与 store 同口径：Windows 比较卷名）。
func sameVolume(a, b string) bool {
	return strings.EqualFold(filepath.VolumeName(a), filepath.VolumeName(b))
}

// Path 返回 blob 落盘路径。
func (b *GlobalBlobs) Path(hash string) string {
	prefix := "xx"
	if len(hash) >= 2 {
		prefix = hash[:2]
	}
	return filepath.Join(b.root, prefix, hash)
}

// Put 写入内容（去重），返回 (hash, size)。
func (b *GlobalBlobs) Put(ctx context.Context, content []byte) (string, int64, error) {
	if int64(len(content)) > MaxSnapshotBytes {
		return "", 0, fmt.Errorf("%w: %d > %d 字节", ErrSnapshotTooLarge, len(content), MaxSnapshotBytes)
	}
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	dst := b.Path(hash)
	if _, err := os.Stat(dst); err == nil {
		return hash, int64(len(content)), nil // 去重命中
	}
	part := filepath.Join(b.tmpDir, "snap-"+newRandomHex()+".part")
	if err := os.WriteFile(part, content, 0o644); err != nil {
		return "", 0, fmt.Errorf("snapshot: 写临时文件失败: %w", err)
	}
	if err := os.Chmod(part, 0o644); err != nil { // Windows 上 WriteFile perm 仅影响新文件
		_ = os.Remove(part)
		return "", 0, fmt.Errorf("snapshot: 修正临时文件权限失败: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(part)
		return "", 0, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		_ = os.Remove(part)
		return "", 0, fmt.Errorf("snapshot: 创建快照分桶目录失败: %w", err)
	}
	if err := os.Rename(part, dst); err != nil {
		_ = os.Remove(part)
		// Windows 上目标已存在会失败：并发写入同内容按去重处理
		if _, statErr := os.Stat(dst); statErr == nil {
			return hash, int64(len(content)), nil
		}
		return "", 0, fmt.Errorf("snapshot: rename 快照失败: %w", err)
	}
	return hash, int64(len(content)), nil
}

// Open 打开 blob 读取流。
func (b *GlobalBlobs) Open(_ context.Context, hash string) (io.ReadCloser, error) {
	if hash == "" {
		return nil, fmt.Errorf("%w: 空 hash", ErrInvalidSnapshot)
	}
	f, err := os.Open(b.Path(hash))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: 快照内容 %s 缺失（CAS 文件不存在）", ErrInvalidSnapshot, hash)
		}
		return nil, fmt.Errorf("snapshot: 打开快照内容失败: %w", err)
	}
	return f, nil
}

// Remove 删除 blob 文件（GC 用；不存在不算错误）。
func (b *GlobalBlobs) Remove(_ context.Context, hash string) error {
	if hash == "" {
		return nil
	}
	if err := os.Remove(b.Path(hash)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("snapshot: 删除快照内容失败: %w", err)
	}
	return nil
}

// newRandomHex 生成 16 字节随机十六进制串。
func newRandomHex() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("t%d", os.Getpid())
	}
	return hex.EncodeToString(buf[:])
}
