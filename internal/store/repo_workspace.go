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

// ErrWorkspaceBound 工作空间仍绑定设备（拒绝删除，必须先解绑归属）。
//
// 为什么单独成哨兵：它与 ErrWorkspaceInUse 的处理方式截然不同——
// 「有引用」可以靠 migrate_refs 一次性迁移落地；「已绑定设备」**不能**靠迁移解决，
// 必须先执行解绑操作（用户要显式选择把空间从哪台机器上摘下来）。
// 混用同一个错误会让上层给出「已迁移引用」这类错误承诺。
var ErrWorkspaceBound = errors.New("store: 工作空间仍绑定设备")

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

// rejectIfBound 若空间仍绑定设备则返回 ErrWorkspaceBound。
//
// 抽成独立方法而不是在 Delete 内联：DeleteMigratingRefs 需要在**事务内**做同一件事
// （那里用内联 SQL 以保持与事务原子），两处的判定口径必须一致，故共用同一句谓词。
func (r *WorkspaceRepo) rejectIfBound(ctx context.Context, id string) error {
	var bound int
	if err := r.s.conn().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workspaces WHERE id=? AND device_id<>''`, id).Scan(&bound); err != nil {
		return fmt.Errorf("store: 查询工作空间绑定状态失败: %w", err)
	}
	if bound > 0 {
		return fmt.Errorf("%w: %s", ErrWorkspaceBound, id)
	}
	return nil
}

// Delete 物理删除工作空间。
//
// 铁律：不做级联删除——若仍有会话或项目引用本空间，返回 ErrWorkspaceInUse，
// 由上层转成 409 让用户先迁移引用（静默级联会让会话丢失执行面归属）。
// 服务端内置空间 ws-server 不可删除（它是回填兜底，删掉会让历史数据悬空）。
// 仍绑定设备的空间不可删除（返回 ErrWorkspaceBound，须先解绑归属）。
func (r *WorkspaceRepo) Delete(ctx context.Context, id string) error {
	if id == BuiltinWorkspaceServerID {
		return errors.New("store: 内置服务端空间不可删除")
	}
	if err := r.rejectIfBound(ctx, id); err != nil {
		return err
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

// DeleteMigratingRefs 级联删除工作空间（用户点「确认移除」时的语义）。
//
// 与 Delete 的区别：Delete 是**严格模式**（有引用就 409），本方法是**一次性级联删除**：
// 在同一个主库事务内把归属本空间的会话与项目一并软删、清掉其项目审批规则，
// 最后物理删除空间行。返回被软删的会话 ID 列表，供上层在事务成功后清理会话物理目录。
//
// 为什么会话/项目走软删、空间走物理删：
//   - 会话/项目已有 deleted_at 列，全站统一软删口径（SessionService.Delete /
//     DeleteProjectCascade），沿用可保持审计链与一致性；
//   - workspaces 表没有 deleted_at（0009 迁移明确无软删），而「删了刷新又出现」
//     的根因正是空间行还在 → 必须物理删。
//
// 🚨 本方法绝不把引用改挂到内置 ws-server：那是 ToolPolicyFull（可写 + 命令），
// 而原空间可能是 mobile（readonly）或设备离线（none），迁过去即静默提权（违反 D8）。
// 级联删除从根本上避免了这条路。
//
// 内置空间 ws-server 不可删（它是历史回填兜底，删掉会让既有引用悬空）。
func (r *WorkspaceRepo) DeleteMigratingRefs(ctx context.Context, id string) ([]string, error) {
	if id == BuiltinWorkspaceServerID {
		return nil, errors.New("store: 内置服务端空间不可删除")
	}
	var sessionIDs []string
	err := r.s.withTx(ctx, func(tx *sql.Tx) error {
		// 先确认空间确实存在：避免对不存在的 ID 静默成功（调用方期望 404）。
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workspaces WHERE id=?`, id).Scan(&exists); err != nil {
			return fmt.Errorf("store: 查询工作空间失败: %w", err)
		}
		if exists == 0 {
			return fmt.Errorf("%w: 工作空间 %s", ErrNotFound, id)
		}

		// 绑定设备的空间拒绝删除：迁移引用解决不了「空间还挂着某台机器」这件事，
		// 必须由用户先显式解绑。校验放在事务内与删除动作原子，避免 TOCTOU。
		var bound int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM workspaces WHERE id=? AND device_id<>''`, id).Scan(&bound); err != nil {
			return fmt.Errorf("store: 查询工作空间绑定状态失败: %w", err)
		}
		if bound > 0 {
			return fmt.Errorf("%w: %s", ErrWorkspaceBound, id)
		}

		// ① 收集待删会话 ID（物理目录清理需要；必须在软删前查出来）。
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM sessions WHERE workspace_id=? AND deleted_at IS NULL`, id)
		if err != nil {
			return fmt.Errorf("store: 查询工作空间会话失败: %w", err)
		}
		for rows.Next() {
			var sid string
			if err := rows.Scan(&sid); err != nil {
				rows.Close()
				return fmt.Errorf("store: 读取会话 ID 失败: %w", err)
			}
			sessionIDs = append(sessionIDs, sid)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: 遍历会话 ID 失败: %w", err)
		}

		now := NowMillis()
		// ② 软删该空间的会话（与全站会话删除口径一致）。
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET deleted_at=?, updated_at=? WHERE workspace_id=? AND deleted_at IS NULL`,
			now, now, id); err != nil {
			return fmt.Errorf("store: 级联删除工作空间会话失败: %w", err)
		}
		// ③ 清掉该空间各项目的审批规则（软删项目后规则不该再被下发）。
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM project_approval_rules WHERE project_id IN (SELECT id FROM projects WHERE workspace_id=?)`,
			id); err != nil {
			return fmt.Errorf("store: 级联清理工作空间项目规则失败: %w", err)
		}
		// ④ 软删该空间的项目。
		if _, err := tx.ExecContext(ctx,
			`UPDATE projects SET deleted_at=?, updated_at=? WHERE workspace_id=? AND deleted_at IS NULL`,
			now, now, id); err != nil {
			return fmt.Errorf("store: 级联删除工作空间项目失败: %w", err)
		}
		// ⑤ 物理删空间行 —— 这是「刷新又出现」的根，必须真的消失。
		res, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id=?`, id)
		if err != nil {
			return fmt.Errorf("store: 删除工作空间失败: %w", err)
		}
		return ensureAffected(res, "工作空间 "+id)
	})
	if err != nil {
		return nil, err
	}
	return sessionIDs, nil
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
