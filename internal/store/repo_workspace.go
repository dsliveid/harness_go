package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"harness-go/internal/model"
)

// BuiltinWorkspaceServerID 服务端内置工作空间 ID（0009 迁移内插入，不可删除）。
//
// 语义：所有历史会话/项目回填到此空间，等价于「改造前的既有行为」，零倒退。
const BuiltinWorkspaceServerID = "ws-server"

// ErrWorkspaceInUse 工作空间仍被会话或项目引用（拒绝删除，不做级联）。
var ErrWorkspaceInUse = errors.New("store: 工作空间仍被引用")

// WorkspaceRepo 主库工作空间仓储（工作空间 = 执行面 / 客户端接入点，能力天花板的依据）。
//
// 与 ProjectRepo 的差异：
//   - 无软删：workspaces 表没有 deleted_at 列，删除即物理删除；
//   - 不做级联：Delete 前必须确认没有会话/项目引用，有引用即报 ErrWorkspaceInUse，
//     交由上层决定（避免把会话静默变成"无执行面"或误提权/降权）。
type WorkspaceRepo struct{ s *MainStore }

// Workspaces 返回主库工作空间仓储。
func (s *MainStore) Workspaces() *WorkspaceRepo { return &WorkspaceRepo{s: s} }

// workspaceColumns 工作空间列清单（与 scanWorkspace 顺序严格一致）。
const workspaceColumns = `id, name, kind, device_id, root_path, created_at, updated_at`

func scanWorkspace(sc interface{ Scan(...any) error }) (model.Workspace, error) {
	var w model.Workspace
	err := sc.Scan(&w.ID, &w.Name, &w.Kind, &w.DeviceID, &w.RootPath, &w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		return model.Workspace{}, err
	}
	return w, nil
}

// Create 新建工作空间。ID 必填；时间戳在此集中裁决（与 ProjectRepo.Create 同风格）。
func (r *WorkspaceRepo) Create(ctx context.Context, w *model.Workspace) error {
	if w.ID == "" {
		return errors.New("store: 工作空间 ID 不能为空")
	}
	now := NowMillis()
	if w.CreatedAt == 0 {
		w.CreatedAt = now
	}
	w.UpdatedAt = now
	_, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO workspaces(id, name, kind, device_id, root_path, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?)`,
		w.ID, w.Name, string(w.Kind), w.DeviceID, w.RootPath, w.CreatedAt, w.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: 创建工作空间失败: %w", err)
	}
	return nil
}

// Get 按 ID 取工作空间（不存在返回 ErrNotFound）。
func (r *WorkspaceRepo) Get(ctx context.Context, id string) (*model.Workspace, error) {
	row := r.s.conn().QueryRowContext(ctx,
		`SELECT `+workspaceColumns+` FROM workspaces WHERE id=?`, id)
	w, err := scanWorkspace(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: 工作空间 %s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("store: 查询工作空间失败: %w", err)
	}
	return &w, nil
}

// List 列出全部工作空间：服务端内置空间永远排最前，其余按创建时间升序。
func (r *WorkspaceRepo) List(ctx context.Context) ([]model.Workspace, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT `+workspaceColumns+` FROM workspaces
		 ORDER BY CASE WHEN kind='server' THEN 0 ELSE 1 END, created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: 列出工作空间失败: %w", err)
	}
	defer rows.Close()
	out := make([]model.Workspace, 0, 8)
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 解析工作空间行失败: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Update 更新名称 / 宿主根目录 / 归属设备（三字段整体覆盖）。
//
// 🚨 kind 不在可更新字段内：kind 是能力天花板依据，改 kind = 提权（服务层先拒绝）。
func (r *WorkspaceRepo) Update(ctx context.Context, id, name, deviceID, rootPath string) error {
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE workspaces SET name=?, device_id=?, root_path=?, updated_at=? WHERE id=?`,
		name, deviceID, rootPath, NowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: 更新工作空间失败: %w", err)
	}
	return ensureAffected(res, "工作空间 "+id)
}

// Delete 物理删除工作空间。
//
// 铁律：不做级联删除——若仍有会话或项目引用本空间，返回 ErrWorkspaceInUse，
// 由上层转成 409 让用户先迁移引用（静默级联会让会话丢失执行面归属）。
// 服务端内置空间 ws-server 不可删除（它是回填兜底，删掉会让历史数据悬空）。
func (r *WorkspaceRepo) Delete(ctx context.Context, id string) error {
	if id == BuiltinWorkspaceServerID {
		return errors.New("store: 内置服务端空间不可删除")
	}
	var sessions, projects int64
	if err := r.s.conn().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE workspace_id=?`, id).Scan(&sessions); err != nil {
		return fmt.Errorf("store: 统计工作空间会话引用失败: %w", err)
	}
	if err := r.s.conn().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM projects WHERE workspace_id=?`, id).Scan(&projects); err != nil {
		return fmt.Errorf("store: 统计工作空间项目引用失败: %w", err)
	}
	if sessions > 0 || projects > 0 {
		return fmt.Errorf("%w: %s（会话 %d 个、项目 %d 个）", ErrWorkspaceInUse, id, sessions, projects)
	}
	res, err := r.s.conn().ExecContext(ctx, `DELETE FROM workspaces WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("store: 删除工作空间失败: %w", err)
	}
	return ensureAffected(res, "工作空间 "+id)
}

// GetByDeviceID 按归属设备反查工作空间（一台设备最多一个空间，取最早创建的一条）。
//
// 语义：device_id 为空串是服务端内置空间，本方法只服务于端侧宿主空间，故空串直接返回 ErrNotFound。
func (r *WorkspaceRepo) GetByDeviceID(ctx context.Context, deviceID string) (*model.Workspace, error) {
	if deviceID == "" {
		return nil, fmt.Errorf("%w: 设备 ID 为空", ErrNotFound)
	}
	row := r.s.conn().QueryRowContext(ctx,
		`SELECT `+workspaceColumns+` FROM workspaces WHERE device_id=? ORDER BY created_at ASC LIMIT 1`,
		deviceID)
	w, err := scanWorkspace(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: 设备 %s 的工作空间", ErrNotFound, deviceID)
		}
		return nil, fmt.Errorf("store: 按设备查询工作空间失败: %w", err)
	}
	return &w, nil
}

// GetByPath 按宿主根目录反查工作空间（取最早创建的一条）。
func (r *WorkspaceRepo) GetByPath(ctx context.Context, rootPath string) (*model.Workspace, error) {
	if rootPath == "" {
		return nil, fmt.Errorf("%w: 根目录为空", ErrNotFound)
	}
	row := r.s.conn().QueryRowContext(ctx,
		`SELECT `+workspaceColumns+` FROM workspaces WHERE root_path=? ORDER BY created_at ASC LIMIT 1`,
		rootPath)
	w, err := scanWorkspace(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: 根目录 %s 的工作空间", ErrNotFound, rootPath)
		}
		return nil, fmt.Errorf("store: 按路径查询工作空间失败: %w", err)
	}
	return &w, nil
}
