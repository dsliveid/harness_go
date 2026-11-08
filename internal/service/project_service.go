package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"harness-go/internal/apperr"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// ProjectService 项目用例：项目 CRUD + 项目级审批规则维护 + 默认控制权限。
//
// 设计要点：
//   - 「默认控制权限」（default_access_mode）是**项目级**的权限继承源，只在新建会话那一刻
//     被 SessionService.Create 快照到会话，不影响既有会话；
//   - access_mode 一律走 normalizeAccessModeStrict 严格校验，非法值 400 拒绝，
//     绝不静默降级（避免用户以为设成了 full 实际却是 confirm）；
//   - 删除项目必须走仓储的 DeleteProjectCascade（事务内软删 + 清规则 + 会话 project_id 归零）。
type ProjectService struct {
	main *store.MainStore
}

// NewProjectService 创建项目用例。
func NewProjectService(main *store.MainStore) *ProjectService {
	return &ProjectService{main: main}
}

// Create 新建项目。
//
//   - defaultAccessMode 空串回落 "confirm"；非空必须合法，否则 400；
//   - name 为空回落 path 的 basename；name 与 path 都空则 400（无意义的空项目）。
func (s *ProjectService) Create(ctx context.Context, name, path, defaultAccessMode string) (*model.Project, error) {
	name = strings.TrimSpace(name)
	path = strings.TrimSpace(path)
	if name == "" && path != "" {
		name = filepath.Base(path)
	}
	if name == "" && path == "" {
		return nil, invalidArg("项目名与路径不能同时为空")
	}
	mode := strings.TrimSpace(defaultAccessMode)
	if mode == "" {
		mode = "confirm"
	} else if normalized := normalizeAccessModeStrict(mode); normalized == "" {
		return nil, invalidArg("default_access_mode 必须是 confirm 或 full")
	} else {
		mode = normalized
	}
	p := &model.Project{
		ID:                "proj-" + newID(),
		Name:              name,
		Path:              path,
		DefaultAccessMode: mode,
	}
	if err := s.main.Projects().Create(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// List 列出全部项目（置顶优先，其次按更新时间倒序）。
func (s *ProjectService) List(ctx context.Context) ([]model.Project, error) {
	return s.main.Projects().List(ctx)
}

// Get 取项目（不存在 404）。
func (s *ProjectService) Get(ctx context.Context, id string) (*model.Project, error) {
	p, err := s.main.Projects().Get(ctx, id)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeNotFound, "项目不存在")
	}
	return p, nil
}

// Update 更新项目。
//
// 语义约定（与 Create 区分）：
//   - name 空串 = 保留原名称（不修改），避免前端只改权限时误清空标题；
//   - defaultAccessMode 空串 = 保留原权限（不修改）；非空必须合法；
//   - pinned 总是按入参覆盖（置顶是明确的布尔开关）。
func (s *ProjectService) Update(ctx context.Context, id, name, defaultAccessMode string, pinned bool) error {
	cur, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	newName := cur.Name
	if v := strings.TrimSpace(name); v != "" {
		newName = v
	}
	newMode := cur.DefaultAccessMode
	if v := strings.TrimSpace(defaultAccessMode); v != "" {
		normalized := normalizeAccessModeStrict(v)
		if normalized == "" {
			return invalidArg("default_access_mode 必须是 confirm 或 full")
		}
		newMode = normalized
	}
	return wrapStoreErr(s.main.Projects().Update(ctx, id, newName, newMode, pinned),
		apperr.CodeNotFound, "项目不存在")
}

// Delete 删除项目（事务级联：软删项目 + 清规则 + 相关会话 project_id 归零）。
func (s *ProjectService) Delete(ctx context.Context, id string) error {
	return wrapStoreErr(s.main.Projects().DeleteProjectCascade(ctx, id),
		apperr.CodeNotFound, "项目不存在")
}

// SetDefaultAccessMode 修改项目默认控制权限（仅影响此后新建的会话）。
func (s *ProjectService) SetDefaultAccessMode(ctx context.Context, id, mode string) error {
	normalized := normalizeAccessModeStrict(mode)
	if normalized == "" {
		return invalidArg("default_access_mode 必须是 confirm 或 full")
	}
	return wrapStoreErr(s.main.Projects().SetDefaultAccessMode(ctx, id, normalized),
		apperr.CodeNotFound, "项目不存在")
}

// ListRules 列出项目审批规则（先校验项目存在，避免对已删项目返回空数组造成误解）。
func (s *ProjectService) ListRules(ctx context.Context, projectID string) ([]model.ProjectApprovalRule, error) {
	if _, err := s.Get(ctx, projectID); err != nil {
		return nil, err
	}
	return s.main.Projects().ListRules(ctx, projectID)
}

// AddRule 新增项目审批规则（toolName 必填；项目必须存在；重复添加幂等返回既有规则）。
func (s *ProjectService) AddRule(ctx context.Context, projectID, toolName, pattern string) (*model.ProjectApprovalRule, error) {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return nil, invalidArg("tool_name 不能为空")
	}
	if _, err := s.Get(ctx, projectID); err != nil {
		return nil, err
	}
	return s.main.Projects().AddRule(ctx, projectID, toolName, strings.TrimSpace(pattern))
}

// DeleteRule 删除项目审批规则（规则不属于该项目时按不存在处理，避免跨项目误删）。
func (s *ProjectService) DeleteRule(ctx context.Context, projectID, ruleID string) error {
	rules, err := s.main.Projects().ListRules(ctx, projectID)
	if err != nil {
		return err
	}
	found := false
	for _, r := range rules {
		if r.ID == ruleID {
			found = true
			break
		}
	}
	if !found {
		return apperr.New(apperr.CodeNotFound, "规则不存在")
	}
	if err := s.main.Projects().DeleteRule(ctx, ruleID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return apperr.New(apperr.CodeNotFound, "规则不存在")
		}
		return err
	}
	return nil
}
