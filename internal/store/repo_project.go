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
const projectColumns = `id, name, path, default_access_mode, pinned, created_at, updated_at, deleted_at`

func scanProject(sc interface{ Scan(...any) error }) (model.Project, error) {
	var (
		p       model.Project
		pinned  int64
		deleted sql.NullInt64
	)
	err := sc.Scan(&p.ID, &p.Name, &p.Path, &p.DefaultAccessMode, &pinned,
		&p.CreatedAt, &p.UpdatedAt, &deleted)
	if err != nil {
		return model.Project{}, err
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
	pinned := 0
	if p.Pinned {
		pinned = 1
	}
	_, err := r.s.conn().ExecContext(ctx,
		`INSERT INTO projects(id, name, path, default_access_mode, pinned, created_at, updated_at, deleted_at)
		 VALUES(?,?,?,?,?,?,?,NULL)`,
		p.ID, p.Name, p.Path, p.DefaultAccessMode, pinned, p.CreatedAt, p.UpdatedAt)
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

// Update 更新项目名称、默认控制权限与置顶状态（三字段整体覆盖）。
func (r *ProjectRepo) Update(ctx context.Context, id, name, defaultAccessMode string, pinned bool) error {
	pinnedVal := 0
	if pinned {
		pinnedVal = 1
	}
	res, err := r.s.conn().ExecContext(ctx,
		`UPDATE projects SET name=?, default_access_mode=?, pinned=?, updated_at=?
		 WHERE id=? AND deleted_at IS NULL`,
		name, defaultAccessMode, pinnedVal, NowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: 更新项目失败: %w", err)
	}
	return ensureAffected(res, "项目 "+id)
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
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET project_id='' WHERE project_id=?`, id); err != nil {
			return fmt.Errorf("store: 级联解绑会话归属失败: %w", err)
		}
		return nil
	})
}
