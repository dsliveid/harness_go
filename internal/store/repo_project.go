package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"harness-go/internal/model"
)

// ProjectRepo 主库项目仓储（项目 = 工作区 + 默认控制权限 + 项目审批规则）。
//
// 铁律：软删（deleted_at）对上层完全透明——Get/List/Rules 一律过滤已删项目；
// 删除项目必须走 DeleteProjectCascade，在一个事务内清掉规则并把会话的 project_id 归零，
// 绝不允许留下悬空引用。
type ProjectRepo struct{ s *MainStore }

// Projects 返回主库项目仓储。
func (s *MainStore) Projects() *ProjectRepo { return &ProjectRepo{s: s} }

// projectColumns 项目列清单（与 scanProject 顺序严格一致）。
// 0009 追加 workspace_id：位置紧随 path 之后（scanProject 实参保持一致）。
// 0014 追加 constraints / plan_mode：位置紧随 pinned 之后（三处必须同步，见下方铁律）。
const projectColumns = `id, name, path, workspace_id, default_access_mode, pinned, constraints, plan_mode, created_at, updated_at, deleted_at`

func scanProject(sc interface{ Scan(...any) error }) (model.Project, error) {
	var (
		p       model.Project
		pinned  int64
		deleted sql.NullInt64
		// workspace_id 由 0009 以可空列加入（回填后为 ws-server），用 NullString 兜底。
		workspaceID sql.NullString
	)
	err := sc.Scan(&p.ID, &p.Name, &p.Path, &workspaceID, &p.DefaultAccessMode, &pinned,
		&p.Constraints, &p.PlanMode,
		&p.CreatedAt, &p.UpdatedAt, &deleted)
	if err != nil {
		return model.Project{}, err
	}
	p.WorkspaceID = workspaceID.String
	if p.WorkspaceID == "" {
		// 未指定空间的旧调用路径一律落户服务端内置空间（与 0009 回填口径一致，
		// 避免出现 workspace_id 为空串的"无归属项目"）。
		p.WorkspaceID = BuiltinWorkspaceServerID
	}
	p.Pinned = pinned != 0
	if deleted.Valid {
		v := deleted.Int64
		p.DeletedAt = &v
	}
	return p, nil
}

// Create 创建项目。ID 必填；时间戳与默认权限在此集中裁决（与 SessionRepo.Create 同风格）。
func (r *ProjectRepo) Create(ctx context.Context, p *model.Project) error {
	if p.ID == "" {
		return errors.New("store: 项目 ID 不能为空")
	}
	now := NowMillis()
	if p.CreatedAt == 0 {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	if p.DefaultAccessMode == "" {
		p.DefaultAccessMode = "confirm"
	}
	// plan_mode 空串回落 standard：与 0014 迁移的列 DEFAULT 保持同一口径，
	// 避免「代码缺省」与「库里缺省」两套默认值。
	if p.PlanMode == "" {
		p.PlanMode = "standard"
	}
	pinned := 0
	if p.Pinned {
		pinned = 1
	}
	_, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO projects(id, name, path, workspace_id, default_access_mode, pinned, constraints, plan_mode,
		                      created_at, updated_at, deleted_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,NULL)`,
		p.ID, p.Name, p.Path, p.WorkspaceID, p.DefaultAccessMode, pinned, p.Constraints, p.PlanMode,
		p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: 创建项目失败: %w", err)
	}
	return nil
}

// Get 按 ID 取项目（已软删视同不存在）。
func (r *ProjectRepo) Get(ctx context.Context, id string) (*model.Project, error) {
	row := r.s.conn().QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE id=? AND deleted_at IS NULL`, id)
	p, err := scanProject(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: 项目 %s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("store: 查询项目失败: %w", err)
	}
	return &p, nil
}

// List 列出全部未删项目（置顶优先，其次按更新时间倒序）。
func (r *ProjectRepo) List(ctx context.Context) ([]model.Project, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE deleted_at IS NULL
		 ORDER BY pinned DESC, updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: 列出项目失败: %w", err)
	}
	defer rows.Close()
	out := make([]model.Project, 0, 16)
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 解析项目行失败: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Update 更新项目的标量设置（名称 / 默认控制权限 / 置顶 / 约束 / 规划模式）。
//
// 0014 起 constraints 与 planMode 也纳入本方法（原先只在前端 localStorage）。
// 调用方（service.ProjectService.Update）负责把「未提供的字段」合并成当前值，
// 本方法只做整体覆盖，语义单一、不与上层重复裁决。
func (r *ProjectRepo) Update(ctx context.Context, p *model.Project) error {
	pinnedVal := 0
	if p.Pinned {
		pinnedVal = 1
	}
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE projects SET name=?, default_access_mode=?, pinned=?, constraints=?, plan_mode=?, updated_at=?
		 WHERE id=? AND deleted_at IS NULL`,
		p.Name, p.DefaultAccessMode, pinnedVal, p.Constraints, p.PlanMode, NowMillis(), p.ID)
	if err != nil {
		return fmt.Errorf("store: 更新项目失败: %w", err)
	}
	return ensureAffected(res, "项目 "+p.ID)
}

// SetDefaultAccessMode 单独更新项目默认控制权限（供"只改权限"的场景使用）。
func (r *ProjectRepo) SetDefaultAccessMode(ctx context.Context, id, mode string) error {
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE projects SET default_access_mode=?, updated_at=? WHERE id=? AND deleted_at IS NULL`,
		mode, NowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: 更新项目默认控制权限失败: %w", err)
	}
	return ensureAffected(res, "项目 "+id)
}

// SoftDelete 软删项目（幂等：已删项目再删返回 ErrNotFound）。
//
// 注意：仅软删项目行本身；规则清理与会话 project_id 归零必须走 DeleteProjectCascade。
func (r *ProjectRepo) SoftDelete(ctx context.Context, id string) error {
	now := NowMillis()
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE projects SET deleted_at=?, updated_at=? WHERE id=? AND deleted_at IS NULL`,
		now, now, id)
	if err != nil {
		return fmt.Errorf("store: 删除项目失败: %w", err)
	}
	return ensureAffected(res, "项目 "+id)
}

// ============ 项目审批规则 ============

// ListRules 列出某项目的全部审批规则（按创建时间升序，保证下发顺序稳定）。
func (r *ProjectRepo) ListRules(ctx context.Context, projectID string) ([]model.ProjectApprovalRule, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT id, project_id, tool_name, pattern, created_at
		 FROM project_approval_rules WHERE project_id=? ORDER BY created_at ASC, id ASC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: 列出项目规则失败: %w", err)
	}
	defer rows.Close()
	out := make([]model.ProjectApprovalRule, 0, 8)
	for rows.Next() {
		var rule model.ProjectApprovalRule
		if err := rows.Scan(&rule.ID, &rule.ProjectID, &rule.ToolName, &rule.Pattern, &rule.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: 解析项目规则行失败: %w", err)
		}
		out = append(out, rule)
	}
	return out, rows.Err()
}

// AddRule 新增项目审批规则（幂等：唯一索引 (project_id, tool_name, pattern) 冲突时
// 不报错、不覆盖，直接查回既有行返回）。
//
// 为什么幂等：规则下发是"声明式"业务，前端重复提交/并发重放不应产生用户可见错误。
func (r *ProjectRepo) AddRule(ctx context.Context, projectID, toolName, pattern string) (*model.ProjectApprovalRule, error) {
	now := NowMillis()
	_, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO project_approval_rules(id, project_id, tool_name, pattern, created_at)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(project_id, tool_name, pattern) DO NOTHING`,
		"rule-"+newUUID(), projectID, toolName, pattern, now)
	if err != nil {
		return nil, fmt.Errorf("store: 新增项目规则失败: %w", err)
	}
	var rule model.ProjectApprovalRule
	err = r.s.conn().QueryRowContext(ctx,
		`SELECT id, project_id, tool_name, pattern, created_at
		 FROM project_approval_rules WHERE project_id=? AND tool_name=? AND pattern=?`,
		projectID, toolName, pattern).
		Scan(&rule.ID, &rule.ProjectID, &rule.ToolName, &rule.Pattern, &rule.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("store: 查回项目规则失败: %w", err)
	}
	return &rule, nil
}

// DeleteRule 删除项目审批规则（不存在返回 ErrNotFound）。
func (r *ProjectRepo) DeleteRule(ctx context.Context, id string) error {
	res, err := r.s.conn().ExecContext(ctx,
		`DELETE FROM project_approval_rules WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("store: 删除项目规则失败: %w", err)
	}
	return ensureAffected(res, "项目规则 "+id)
}

// DeleteProjectCascade 事务式删除项目：软删项目 + 物理删规则 + 会话 project_id 归零。
//
// 三件事必须在同一个主库事务内完成，否则会出现「项目没了但会话仍指向它」的悬空引用。
// 项目不存在（或已软删）时返回 ErrNotFound 且不产生任何副作用。
func (r *ProjectRepo) DeleteProjectCascade(ctx context.Context, id string) error {
	return r.s.withTx(ctx, func(tx *sql.Tx) error {
		now := NowMillis()
		res, err := tx.ExecContext(ctx,
			`UPDATE projects SET deleted_at=?, updated_at=? WHERE id=? AND deleted_at IS NULL`,
			now, now, id)
		if err != nil {
			return fmt.Errorf("store: 级联删除项目失败: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: 读取影响行数失败: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("%w: 项目 %s", ErrNotFound, id)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM project_approval_rules WHERE project_id=?`, id); err != nil {
			return fmt.Errorf("store: 级联清理项目规则失败: %w", err)
		}
		// 0014：关联项目与数据权限也属项目子集合，必须一并物理清理，
		// 否则重建同名/同 id 项目时会「继承」上一个项目的幽灵配置。
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM project_links WHERE project_id=?`, id); err != nil {
			return fmt.Errorf("store: 级联清理关联项目失败: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM project_data_grants WHERE project_id=?`, id); err != nil {
			return fmt.Errorf("store: 级联清理数据权限失败: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET project_id='' WHERE project_id=?`, id); err != nil {
			return fmt.Errorf("store: 级联解绑会话归属失败: %w", err)
		}
		return nil
	})
}

// ============ 关联项目（0014） ============

// ListLinks 列出某项目的全部关联项目（按创建时间升序，保证展示顺序稳定）。
func (r *ProjectRepo) ListLinks(ctx context.Context, projectID string) ([]model.ProjectLink, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT id, project_id, path, description, created_at
		 FROM project_links WHERE project_id=? ORDER BY created_at ASC, id ASC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: 列出关联项目失败: %w", err)
	}
	defer rows.Close()
	out := make([]model.ProjectLink, 0, 8)
	for rows.Next() {
		var l model.ProjectLink
		if err := rows.Scan(&l.ID, &l.ProjectID, &l.Path, &l.Description, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: 解析关联项目行失败: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// AddLink 新增关联项目（id 由本层生成）。
func (r *ProjectRepo) AddLink(ctx context.Context, projectID, path, description string) (*model.ProjectLink, error) {
	l := model.ProjectLink{
		ID:          "plink-" + newUUID(),
		ProjectID:   projectID,
		Path:        path,
		Description: description,
		CreatedAt:   NowMillis(),
	}
	if _, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO project_links(id, project_id, path, description, created_at) VALUES(?,?,?,?,?)`,
		l.ID, l.ProjectID, l.Path, l.Description, l.CreatedAt); err != nil {
		return nil, fmt.Errorf("store: 新增关联项目失败: %w", err)
	}
	return &l, nil
}

// UpdateLink 更新关联项目的路径与说明（不存在返回 ErrNotFound）。
func (r *ProjectRepo) UpdateLink(ctx context.Context, id, path, description string) error {
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE project_links SET path=?, description=? WHERE id=?`, path, description, id)
	if err != nil {
		return fmt.Errorf("store: 更新关联项目失败: %w", err)
	}
	return ensureAffected(res, "关联项目 "+id)
}

// DeleteLink 删除关联项目（不存在返回 ErrNotFound）。
func (r *ProjectRepo) DeleteLink(ctx context.Context, id string) error {
	res, err := r.s.conn().ExecContext(ctx, `DELETE FROM project_links WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("store: 删除关联项目失败: %w", err)
	}
	return ensureAffected(res, "关联项目 "+id)
}

// ============ 数据权限策略（0014） ============

// ListDataGrants 列出某项目已显式设置的数据权限行。
//
// 🚨 返回结果**只含用户明确设置过的 scope**；未出现的 scope 由上层按 ask 兜底。
func (r *ProjectRepo) ListDataGrants(ctx context.Context, projectID string) ([]model.ProjectDataGrant, error) {
	rows, err := r.s.conn().QueryContext(ctx,
		`SELECT project_id, scope, mode, updated_at
		 FROM project_data_grants WHERE project_id=? ORDER BY scope ASC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: 列出数据权限失败: %w", err)
	}
	defer rows.Close()
	out := make([]model.ProjectDataGrant, 0, 4)
	for rows.Next() {
		var g model.ProjectDataGrant
		if err := rows.Scan(&g.ProjectID, &g.Scope, &g.Mode, &g.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: 解析数据权限行失败: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetDataGrant 幂等 upsert 某项目某 scope 的数据权限策略。
//
// 用 ON CONFLICT 而非「先查后写」：并发重复提交不应产生报错或双行。
func (r *ProjectRepo) SetDataGrant(ctx context.Context, projectID, scope, mode string) error {
	_, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO project_data_grants(project_id, scope, mode, updated_at) VALUES(?,?,?,?)
		 ON CONFLICT(project_id, scope) DO UPDATE SET mode=excluded.mode, updated_at=excluded.updated_at`,
		projectID, scope, mode, NowMillis())
	if err != nil {
		return fmt.Errorf("store: 写入数据权限失败: %w", err)
	}
	return nil
}
