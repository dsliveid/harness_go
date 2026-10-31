package snapshot

// Store：toolapi.SnapshotStore + SnapshotRegistrar 的内核实装（阶段 G）。
//
// 锚点来源：插件把收到的 ctx 原样传入（internal/tools.Runner 已注入
// toolapi.CallInfo），本包据此把快照与 session/run/tool_event 关联，
// 回滚服务才能按 消息/轮次/工具级 定位快照。

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"harness-go/internal/model"
	"harness-go/internal/store"
	"harness-go/pkg/toolapi"
)

// Options Store 依赖。
type Options struct {
	// Main 主库（快照行 + blob 引用计数）。
	Main *store.MainStore
	// DataDir 数据目录（全局 CAS 挂 data/snapshots/blobs，tmp 挂 data/tmp）。
	DataDir string
	// WorkspaceRoot 工作区绝对根（快照/恢复的路径沙箱基准）。
	WorkspaceRoot string
	// Now 时钟（测试注入）。
	Now func() time.Time
}

// Store 快照存储。
type Store struct {
	blobs *GlobalBlobs
	repo  *store.SnapshotsRepo
	root  string
	tmp   string
	now   func() time.Time
}

// New 创建快照存储。
func New(opts Options) (*Store, error) {
	blobs, err := NewGlobalBlobs(opts.DataDir)
	if err != nil {
		return nil, err
	}
	root := opts.WorkspaceRoot
	if root == "" {
		return nil, fmt.Errorf("snapshot: 工作区根不能为空")
	}
	if abs, aErr := filepath.Abs(root); aErr == nil {
		root = abs
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Store{
		blobs: blobs,
		repo:  opts.Main.Snapshots(),
		root:  root,
		tmp:   filepath.Join(opts.DataDir, "tmp"),
		now:   now,
	}, nil
}

// 编译期断言：同时满足冻结契约与锚点扩展。
var (
	_ toolapi.SnapshotStore     = (*Store)(nil)
	_ toolapi.SnapshotRegistrar = (*Store)(nil)
)

// Create 实现 toolapi.SnapshotStore（语义 = before 锚点）。
func (s *Store) Create(ctx context.Context, paths []string) (toolapi.SnapshotRef, error) {
	return s.CreateAnchored(ctx, paths, toolapi.SnapshotOpt{Kind: model.SnapshotKindBefore})
}

// CreateAnchored 实现 toolapi.SnapshotRegistrar。
//
// 对每个路径读取当前内容（不存在记 absent），内容落全局 CAS，
// 快照行（含锚点）落主库。顺序：先文件后库（与 blob 同铁律）。
func (s *Store) CreateAnchored(ctx context.Context, paths []string, opt toolapi.SnapshotOpt) (toolapi.SnapshotRef, error) {
	if len(paths) == 0 {
		return toolapi.SnapshotRef{}, fmt.Errorf("%w: 快照路径不能为空", toolapi.ErrInvalidArgs)
	}
	kind := opt.Kind
	if kind == "" {
		kind = model.SnapshotKindBefore
	}
	info := toolapi.CallInfoFrom(ctx)

	bindings := make([]model.SnapshotBinding, 0, len(paths))
	var total int64
	seen := map[string]struct{}{}
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		abs, err := s.resolve(p)
		if err != nil {
			return toolapi.SnapshotRef{}, err
		}
		if _, dup := seen[abs]; dup {
			continue
		}
		seen[abs] = struct{}{}
		content, statErr := os.ReadFile(abs)
		switch {
		case statErr == nil:
			hash, size, putErr := s.blobs.Put(ctx, content)
			if putErr != nil {
				return toolapi.SnapshotRef{}, putErr
			}
			bindings = append(bindings, model.SnapshotBinding{Path: s.rel(abs), Hash: hash, Size: size})
			total += size
		case os.IsNotExist(statErr):
			bindings = append(bindings, model.SnapshotBinding{Path: s.rel(abs), Absent: true})
		default:
			return toolapi.SnapshotRef{}, fmt.Errorf("snapshot: 读取 %s 失败: %w", p, statErr)
		}
	}
	if len(bindings) == 0 {
		return toolapi.SnapshotRef{}, fmt.Errorf("%w: 快照路径为空", toolapi.ErrInvalidArgs)
	}

	snap := model.Snapshot{
		ID:          newSnapshotID(),
		SessionID:   info.SessionID,
		RunID:       info.RunID,
		ToolEventID: info.ToolEventID,
		TaskID:      opt.TaskID,
		Kind:        kind,
		Paths:       bindings,
		Size:        total,
		CreatedAt:   s.now().UnixMilli(),
	}
	if err := s.repo.Insert(ctx, &snap); err != nil {
		return toolapi.SnapshotRef{}, fmt.Errorf("snapshot: 登记快照失败: %w", err)
	}
	return toRef(snap), nil
}

// Restore 实现 toolapi.SnapshotStore：把快照引用的每一路径**物理写回**。
//
// ref.Paths 为空时按 ID 从主库补全（内核侧允许只带 ID 恢复）；
// absent 绑定恢复为「文件不存在」（物理删除）。
func (s *Store) Restore(ctx context.Context, ref toolapi.SnapshotRef) error {
	snap, err := s.repo.Get(ctx, ref.ID)
	if err != nil {
		return fmt.Errorf("snapshot: 读取快照 %s 失败: %w", ref.ID, err)
	}
	for _, b := range snap.Paths {
		abs, rErr := s.resolve(b.Path)
		if rErr != nil {
			return rErr
		}
		if b.Absent {
			if rmErr := os.Remove(abs); rmErr != nil && !os.IsNotExist(rmErr) {
				return fmt.Errorf("snapshot: 删除 %s 失败: %w", b.Path, rmErr)
			}
			continue
		}
		rc, openErr := s.blobs.Open(ctx, b.Hash)
		if openErr != nil {
			return openErr
		}
		content, readErr := io.ReadAll(rc)
		_ = rc.Close()
		if readErr != nil {
			return fmt.Errorf("snapshot: 读取快照内容失败: %w", readErr)
		}
		if wErr := s.atomicWrite(abs, content); wErr != nil {
			return wErr
		}
	}
	return nil
}

// List 实现 toolapi.SnapshotStore：全局最近快照（时间倒序）。
// 会话级精确列表由回滚服务 / API 直接走 ListBySession。
func (s *Store) List(ctx context.Context) ([]toolapi.SnapshotRef, error) {
	snaps, err := s.repo.ListRecent(ctx, 100)
	if err != nil {
		return nil, err
	}
	out := make([]toolapi.SnapshotRef, 0, len(snaps))
	for _, snap := range snaps {
		out = append(out, toRef(snap))
	}
	return out, nil
}

// RestorePath 把快照中**单一路径**物理写回（回滚服务按 path 粒度编排用；
// absent 绑定恢复为「文件不存在」）。返回写回结果。
func (s *Store) RestorePath(ctx context.Context, snapshotID, path string) (model.RollbackRestored, error) {
	snap, err := s.repo.Get(ctx, snapshotID)
	if err != nil {
		return model.RollbackRestored{}, fmt.Errorf("snapshot: 读取快照 %s 失败: %w", snapshotID, err)
	}
	out := model.RollbackRestored{Path: filepath.ToSlash(filepath.Clean(path))}
	for _, b := range snap.Paths {
		if b.Path != out.Path {
			continue
		}
		abs, rErr := s.resolve(b.Path)
		if rErr != nil {
			return model.RollbackRestored{}, rErr
		}
		if b.Absent {
			if rmErr := os.Remove(abs); rmErr != nil && !os.IsNotExist(rmErr) {
				return model.RollbackRestored{}, fmt.Errorf("snapshot: 删除 %s 失败: %w", b.Path, rmErr)
			}
			out.Action = "deleted"
			return out, nil
		}
		rc, openErr := s.blobs.Open(ctx, b.Hash)
		if openErr != nil {
			return model.RollbackRestored{}, openErr
		}
		content, readErr := io.ReadAll(rc)
		_ = rc.Close()
		if readErr != nil {
			return model.RollbackRestored{}, fmt.Errorf("snapshot: 读取快照内容失败: %w", readErr)
		}
		if wErr := s.atomicWrite(abs, content); wErr != nil {
			return model.RollbackRestored{}, wErr
		}
		out.Action = "restored"
		out.Bytes = b.Size
		return out, nil
	}
	return model.RollbackRestored{}, fmt.Errorf("%w: 快照 %s 不含路径 %s", ErrInvalidSnapshot, snapshotID, path)
}

// Get 读取快照行（回滚服务用）。
func (s *Store) Get(ctx context.Context, id string) (model.Snapshot, error) {
	return s.repo.Get(ctx, id)
}

// ListBySession 按会话列快照（回滚服务 / API 用）。
func (s *Store) ListBySession(ctx context.Context, sessionID string, limit int) ([]model.Snapshot, error) {
	return s.repo.ListBySession(ctx, sessionID, limit)
}

// ForgetSession 删除会话的全部快照行（会话删除钩子）并归还引用计数。
// 返回引用归零的 blob（建议随后调 GC 物理清理）。
func (s *Store) ForgetSession(ctx context.Context, sessionID string) ([]string, error) {
	return s.repo.DeleteBySession(ctx, sessionID)
}

// Delete 删除单条快照行（补偿回退用）并归还引用计数。
func (s *Store) Delete(ctx context.Context, id string) ([]string, error) {
	return s.repo.Delete(ctx, id)
}

// GC 物理删除引用计数归零的快照内容。
func (s *Store) GC(ctx context.Context) (int, error) {
	hashes, err := s.repo.ZeroRefBlobs(ctx)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, h := range hashes {
		if rErr := s.blobs.Remove(ctx, h); rErr != nil {
			return removed, rErr
		}
		removed++
	}
	if removed > 0 {
		if _, dErr := s.repo.ForgetZeroRefs(ctx); dErr != nil {
			return removed, dErr
		}
	}
	return removed, nil
}

// resolve 把工作区相对路径解析为绝对路径（拒绝绝对路径与越界，
// 与 hostimpl.Workspace.Resolve 同口径——快照与恢复共用一套沙箱规则）。
func (s *Store) resolve(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" || rel == "." {
		return "", fmt.Errorf("%w: 快照路径不能为工作区根", toolapi.ErrInvalidArgs)
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: 不允许绝对路径 %q", toolapi.ErrInvalidArgs, rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: 路径越界 %q", toolapi.ErrInvalidArgs, rel)
	}
	abs := filepath.Join(s.root, clean)
	rootWithSep := s.root + string(filepath.Separator)
	if abs != s.root && !strings.HasPrefix(abs, rootWithSep) {
		return "", fmt.Errorf("%w: 路径越界 %q", toolapi.ErrInvalidArgs, rel)
	}
	return abs, nil
}

// rel 绝对路径 → 工作区相对（slash 形态，快照行的存储口径）。
func (s *Store) rel(abs string) string {
	r, err := filepath.Rel(s.root, abs)
	if err != nil {
		return filepath.ToSlash(abs)
	}
	return filepath.ToSlash(r)
}

// atomicWrite tmp→rename 原子写回（恢复的物理写路径）。
func (s *Store) atomicWrite(abs string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("snapshot: 创建目标目录失败: %w", err)
	}
	part := filepath.Join(s.tmp, "restore-"+newRandomHex()+".part")
	if err := os.WriteFile(part, content, 0o644); err != nil {
		return fmt.Errorf("snapshot: 写恢复临时文件失败: %w", err)
	}
	if err := os.Rename(part, abs); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("snapshot: 恢复写回失败: %w", err)
	}
	return nil
}

func newSnapshotID() string { return "snap-" + newRandomHex() }

func toRef(snap model.Snapshot) toolapi.SnapshotRef {
	paths := make([]string, 0, len(snap.Paths))
	for _, b := range snap.Paths {
		paths = append(paths, b.Path)
	}
	return toolapi.SnapshotRef{
		ID:        snap.ID,
		Paths:     paths,
		Size:      snap.Size,
		CreatedAt: time.UnixMilli(snap.CreatedAt),
	}
}
