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

// UpdateProjectInput 项目标量设置补丁（nil = 不修改）。
//
// 用指针而非「空串表示不修改」的原因：constraints 的**显式清空**是合法操作
// （用户删掉全部约束正文），若沿用空串=不修改的约定，用户就永远清不掉约束。
// 这是本次持久化改造必须区分开的两种语义。
type UpdateProjectInput struct {
	Name              *string
	DefaultAccessMode *string
	Pinned            *bool
	Constraints       *string
	PlanMode          *string
}

// Update 更新项目标量设置（名称 / 默认控制权限 / 置顶 / 约束 / 规划模式）。
//
// 语义约定：
//   - 任一字段为 nil = 保留原值（PATCH 语义，避免改 A 把 B 清空）；
//   - defaultAccessMode / planMode 非 nil 时必须是合法枚举，否则 400（绝不静默降级）；
//   - Constraints 为 nil 保留，**非 nil 即使是空串也照写**（用户要能清空约束）。
func (s *ProjectService) Update(ctx context.Context, id string, in UpdateProjectInput) error {
	cur, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	next := *cur // 以当前值为底，逐字段覆盖

	if in.Name != nil {
		if v := strings.TrimSpace(*in.Name); v != "" {
			// 空名字直接忽略（回落原值）：项目名是标识，不允许被清成空串。
			next.Name = v
		}
	}
	if in.DefaultAccessMode != nil {
		normalized := normalizeAccessModeStrict(strings.TrimSpace(*in.DefaultAccessMode))
		if normalized == "" {
			return invalidArg("default_access_mode 必须是 confirm 或 full")
		}
		next.DefaultAccessMode = normalized
	}
	if in.Pinned != nil {
		next.Pinned = *in.Pinned
	}
	if in.Constraints != nil {
		next.Constraints = *in.Constraints
	}
	if in.PlanMode != nil {
		normalized := normalizePlanModeStrict(strings.TrimSpace(*in.PlanMode))
		if normalized == "" {
			return invalidArg("plan_mode 必须是 standard / always_plan / always_proceed")
		}
		next.PlanMode = normalized
	}
	return wrapStoreErr(s.main.Projects().Update(ctx, &next),
		apperr.CodeNotFound, "项目不存在")
}

// normalizePlanModeStrict 严格归一化规划模式：仅接受三个合法值，其余返回空串。
//
// 与 normalizeAccessModeStrict 同口径：非法值一律由调用方以 400 拒绝，
// 绝不静默降级成 standard —— 用户以为设了「始终先出方案」实际却是标准模式，
// 这种静默降级比直接报错危险得多。
func normalizePlanModeStrict(mode string) string {
	switch mode {
	case "standard", "always_plan", "always_proceed":
		return mode
	default:
		return ""
	}
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

// ============ 关联项目（0014 起落库，原先仅前端 localStorage） ============

// ListLinks 列出项目的全部关联项目（先校验项目存在）。
func (s *ProjectService) ListLinks(ctx context.Context, projectID string) ([]model.ProjectLink, error) {
	if _, err := s.Get(ctx, projectID); err != nil {
		return nil, err
	}
	return s.main.Projects().ListLinks(ctx, projectID)
}

// AddLink 新增关联项目（路径必填；项目必须存在）。
func (s *ProjectService) AddLink(ctx context.Context, projectID, path, description string) (*model.ProjectLink, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, invalidArg("关联目录路径不能为空")
	}
	if _, err := s.Get(ctx, projectID); err != nil {
		return nil, err
	}
	return s.main.Projects().AddLink(ctx, projectID, path, strings.TrimSpace(description))
}

// UpdateLink 更新关联项目（必须属于该项目，避免跨项目误改）。
func (s *ProjectService) UpdateLink(ctx context.Context, projectID, linkID, path, description string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return invalidArg("关联目录路径不能为空")
	}
	links, err := s.main.Projects().ListLinks(ctx, projectID)
	if err != nil {
		return err
	}
	if !hasLink(links, linkID) {
		return apperr.New(apperr.CodeNotFound, "关联项目不存在")
	}
	return wrapStoreErr(s.main.Projects().UpdateLink(ctx, linkID, path, strings.TrimSpace(description)),
		apperr.CodeNotFound, "关联项目不存在")
}

// DeleteLink 删除关联项目（必须属于该项目，避免跨项目误删）。
func (s *ProjectService) DeleteLink(ctx context.Context, projectID, linkID string) error {
	links, err := s.main.Projects().ListLinks(ctx, projectID)
	if err != nil {
		return err
	}
	if !hasLink(links, linkID) {
		return apperr.New(apperr.CodeNotFound, "关联项目不存在")
	}
	return wrapStoreErr(s.main.Projects().DeleteLink(ctx, linkID),
		apperr.CodeNotFound, "关联项目不存在")
}

func hasLink(links []model.ProjectLink, id string) bool {
	for _, l := range links {
		if l.ID == id {
			return true
		}
	}
	return false
}

// ============ 数据权限策略（0014 起落库，原先仅前端内存、刷新即丢） ============

// validDataScopes 合法数据范围（与前端 lib/dataScopes.ts 的 DATA_SCOPES 逐字一致）。
var validDataScopes = map[string]bool{
	"fs_outside":     true, // 工作区外路径
	"session_export": true, // 会话数据导出
	"db_query":       true, // 数据库查询
	"net_egress":     true, // 外部数据外发
}

// validDataModes 合法策略档位：拒绝 / 每次询问 / 直接允许。
var validDataModes = map[string]bool{
	"deny":  true,
	"ask":   true,
	"allow": true,
}

// ListDataGrants 列出项目已显式设置的数据权限（未出现的 scope 由前端按 ask 兜底）。
func (s *ProjectService) ListDataGrants(ctx context.Context, projectID string) ([]model.ProjectDataGrant, error) {
	if _, err := s.Get(ctx, projectID); err != nil {
		return nil, err
	}
	return s.main.Projects().ListDataGrants(ctx, projectID)
}

// SetDataGrant 设置某项目某数据范围的策略（幂等 upsert；枚举非法一律 400）。
//
// 为什么严格校验枚举：数据权限直接决定「越界数据是弹审批还是自动放行」，
// 非法值若被静默接受，界面显示已设置、实际判定却走兜底分支，属安全语义漂移。
func (s *ProjectService) SetDataGrant(ctx context.Context, projectID, scope, mode string) error {
	scope = strings.TrimSpace(scope)
	mode = strings.TrimSpace(mode)
	if !validDataScopes[scope] {
		return invalidArg("scope 必须是 fs_outside / session_export / db_query / net_egress")
	}
	if !validDataModes[mode] {
		return invalidArg("mode 必须是 deny / ask / allow")
	}
	if _, err := s.Get(ctx, projectID); err != nil {
		return err
	}
	return wrapStoreErr(s.main.Projects().SetDataGrant(ctx, projectID, scope, mode),
		apperr.CodeNotFound, "项目不存在")
}
