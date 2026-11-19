package api

import (
	"net/http"

	"harness-go/internal/apperr"
	"harness-go/internal/service"
)

// 本文件：项目域 HTTP 端点（项目 CRUD + 项目审批规则 + 默认控制权限）。
//
// 设计要点：
//   - 传输层只做 JSON 形状转换与参数校验，业务裁决全在 service.ProjectService；
//   - Projects 依赖为 nil 时（未装配项目域）统一返回 503 unavailable，
//     与 Webhooks/Bundles 等可选子系统的既有约定保持一致。

// projectsUnavailableErr 项目域未装配时的统一错误。
func projectsUnavailableErr() error {
	return apperr.New(apperr.CodeUnavailable, "项目域未启用")
}

// ============ 项目 CRUD ============

type createProjectReq struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// DefaultAccessMode 可选：新会话继承的默认控制权限（confirm | full），留空回落 confirm。
	DefaultAccessMode string `json:"default_access_mode"`
}

// listProjects 项目列表。
//
//	GET /api/v1/projects
func (h *handlers) listProjects(w http.ResponseWriter, r *http.Request) {
	if h.d.Projects == nil {
		writeError(w, r, projectsUnavailableErr())
		return
	}
	items, err := h.d.Projects.List(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// createProject 新建项目。
//
//	POST /api/v1/projects
func (h *handlers) createProject(w http.ResponseWriter, r *http.Request) {
	if h.d.Projects == nil {
		writeError(w, r, projectsUnavailableErr())
		return
	}
	// 移动端 WebView 不得创建项目（2026-11 产品决策：移动端只做纯对话）。
	//
	// 🚨 这里必须是**服务端**强制，不能只靠前端 `mobileCapabilities.createProject`
	// 隐藏按钮 —— 前端声明可被任意脚本篡改，只是 UX 提示层（见 mobileCapabilities.ts
	// 文件头「铁律二：不是安全边界」）。真正守住这条线的是本函数。
	//
	// 用令牌 kind 判定（`currentTokenKind`），与 createSession 的能力天花板约束
	// 同一口径 —— 两处若各写一套判定，必然在后续演进中漂移，出现「会话禁了但项目没禁」
	// 这种半开口子。
	if h.currentTokenKind(r) == tokenKindWebview {
		writeError(w, r, apperr.New(apperr.CodeForbidden,
			"移动端不支持创建项目，请到电脑端操作"))
		return
	}
	var req createProjectReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	p, err := h.d.Projects.Create(r.Context(), req.Name, req.Path, req.DefaultAccessMode)
	if err != nil {
		writeError(w, r, err)
		return
	}
	h.publishProjectsChanged()
	writeJSON(w, http.StatusCreated, p)
}

// getProject 取单个项目。
//
//	GET /api/v1/projects/{pid}
func (h *handlers) getProject(w http.ResponseWriter, r *http.Request) {
	if h.d.Projects == nil {
		writeError(w, r, projectsUnavailableErr())
		return
	}
	p, err := h.d.Projects.Get(r.Context(), r.PathValue("pid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// updateProjectReq PATCH /api/v1/projects/{pid} 入参（均可选）。
//
// 全部字段用指针区分「未传」与「显式设置」：
//   - 只改名字时不能把 default_access_mode 重置成 confirm（会静默放大/缩小权限）；
//   - **constraints 必须能显式清空**（传空串 = 清掉约束正文），用指针才能与「未传」区分，
//     否则用户永远删不掉已写的约束。
type updateProjectReq struct {
	Name *string `json:"name"`
	// DefaultAccessMode 非 nil 时更新项目默认控制权限（仅影响此后新建的会话）。
	DefaultAccessMode *string `json:"default_access_mode"`
	Pinned            *bool   `json:"pinned"`
	// Constraints 项目约束正文（0014 起落库）。
	Constraints *string `json:"constraints"`
	// PlanMode 规划与执行策略：standard | always_plan | always_proceed（0014 起落库）。
	PlanMode *string `json:"plan_mode"`
}

// updateProject 更新项目标量设置（名称 / 默认控制权限 / 置顶 / 约束 / 规划模式）。
//
//	PATCH /api/v1/projects/{pid}
//
// 注意：项目路径（path）创建后不再变更——它是会话工作区的配置源，
// 改目录应新建项目而非原地改，避免历史会话产生歧义。
//
// 合并口径统一交给 service.Update（PATCH 语义：nil 保留原值），
// 本层不再手工拼「当前值 + 入参」，避免两处各写一套默认值逻辑而漂移。
func (h *handlers) updateProject(w http.ResponseWriter, r *http.Request) {
	if h.d.Projects == nil {
		writeError(w, r, projectsUnavailableErr())
		return
	}
	var req updateProjectReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if err := h.d.Projects.Update(r.Context(), r.PathValue("pid"), service.UpdateProjectInput{
		Name:              req.Name,
		DefaultAccessMode: req.DefaultAccessMode,
		Pinned:            req.Pinned,
		Constraints:       req.Constraints,
		PlanMode:          req.PlanMode,
	}); err != nil {
		writeError(w, r, err)
		return
	}
	h.publishProjectsChanged()
	w.WriteHeader(http.StatusNoContent)
}

// deleteProject 删除项目（事务级联：软删 + 清规则 + 会话归属归零）。
//
//	DELETE /api/v1/projects/{pid}
func (h *handlers) deleteProject(w http.ResponseWriter, r *http.Request) {
	if h.d.Projects == nil {
		writeError(w, r, projectsUnavailableErr())
		return
	}
	if err := h.d.Projects.Delete(r.Context(), r.PathValue("pid")); err != nil {
		writeError(w, r, err)
		return
	}
	// 删除项目会在事务内核级联把会话的 project_id 归零 → 会话列表也变了。
	// 这里只发项目信号：会话侧的收敛由前端 loadProjects 之后的既有逻辑处理
	// （项目没了，会话归属自然反查不到，不会出现悬空引用）。
	h.publishProjectsChanged()
	w.WriteHeader(http.StatusNoContent)
}

// ============ 项目审批规则 ============

// listProjectRules 列出项目审批规则。
//
//	GET /api/v1/projects/{pid}/approval-rules
func (h *handlers) listProjectRules(w http.ResponseWriter, r *http.Request) {
	if h.d.Projects == nil {
		writeError(w, r, projectsUnavailableErr())
		return
	}
	items, err := h.d.Projects.ListRules(r.Context(), r.PathValue("pid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type addProjectRuleReq struct {
	ToolName string `json:"tool_name"`
	Pattern  string `json:"pattern"`
}

// addProjectRule 新增项目审批规则（命中即直接放行，不进审批闸门）。
//
//	POST /api/v1/projects/{pid}/approval-rules
func (h *handlers) addProjectRule(w http.ResponseWriter, r *http.Request) {
	if h.d.Projects == nil {
		writeError(w, r, projectsUnavailableErr())
		return
	}
	var req addProjectRuleReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	rule, err := h.d.Projects.AddRule(r.Context(), r.PathValue("pid"), req.ToolName, req.Pattern)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

// deleteProjectRule 删除项目审批规则。
//
//	DELETE /api/v1/projects/{pid}/approval-rules/{rid}
func (h *handlers) deleteProjectRule(w http.ResponseWriter, r *http.Request) {
	if h.d.Projects == nil {
		writeError(w, r, projectsUnavailableErr())
		return
	}
	if err := h.d.Projects.DeleteRule(r.Context(), r.PathValue("pid"), r.PathValue("rid")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ============ 关联项目（0014 起落库，原先仅前端 localStorage） ============

// listProjectLinks 列出项目的关联项目。
//
//	GET /api/v1/projects/{pid}/links
func (h *handlers) listProjectLinks(w http.ResponseWriter, r *http.Request) {
	if h.d.Projects == nil {
		writeError(w, r, projectsUnavailableErr())
		return
	}
	items, err := h.d.Projects.ListLinks(r.Context(), r.PathValue("pid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type projectLinkReq struct {
	Path        string `json:"path"`
	Description string `json:"description"`
}

// addProjectLink 新增关联项目。
//
//	POST /api/v1/projects/{pid}/links
func (h *handlers) addProjectLink(w http.ResponseWriter, r *http.Request) {
	if h.d.Projects == nil {
		writeError(w, r, projectsUnavailableErr())
		return
	}
	var req projectLinkReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	link, err := h.d.Projects.AddLink(r.Context(), r.PathValue("pid"), req.Path, req.Description)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, link)
}

// updateProjectLink 更新关联项目（路径 + 说明整体覆盖）。
//
//	PATCH /api/v1/projects/{pid}/links/{lid}
func (h *handlers) updateProjectLink(w http.ResponseWriter, r *http.Request) {
	if h.d.Projects == nil {
		writeError(w, r, projectsUnavailableErr())
		return
	}
	var req projectLinkReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if err := h.d.Projects.UpdateLink(r.Context(), r.PathValue("pid"), r.PathValue("lid"),
		req.Path, req.Description); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteProjectLink 删除关联项目。
//
//	DELETE /api/v1/projects/{pid}/links/{lid}
func (h *handlers) deleteProjectLink(w http.ResponseWriter, r *http.Request) {
	if h.d.Projects == nil {
		writeError(w, r, projectsUnavailableErr())
		return
	}
	if err := h.d.Projects.DeleteLink(r.Context(), r.PathValue("pid"), r.PathValue("lid")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ============ 数据权限策略（0014 起落库，原先仅前端内存、刷新即丢） ============

// listProjectDataGrants 列出项目已显式设置的数据权限。
//
//	GET /api/v1/projects/{pid}/data-grants
//
// 返回**只含用户明确设置过的 scope**；前端对未出现的 scope 按 ask 兜底。
func (h *handlers) listProjectDataGrants(w http.ResponseWriter, r *http.Request) {
	if h.d.Projects == nil {
		writeError(w, r, projectsUnavailableErr())
		return
	}
	items, err := h.d.Projects.ListDataGrants(r.Context(), r.PathValue("pid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type setDataGrantReq struct {
	Mode string `json:"mode"`
}

// setProjectDataGrant 设置某数据范围的策略（幂等 upsert）。
//
//	PUT /api/v1/projects/{pid}/data-grants/{scope}
//
// 用 PUT 而非 PATCH：语义是「把该 scope 的策略设为 mode」，重复提交结果一致。
func (h *handlers) setProjectDataGrant(w http.ResponseWriter, r *http.Request) {
	if h.d.Projects == nil {
		writeError(w, r, projectsUnavailableErr())
		return
	}
	var req setDataGrantReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if err := h.d.Projects.SetDataGrant(r.Context(), r.PathValue("pid"), r.PathValue("scope"), req.Mode); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// _ 保持 strings 引用（部分校验在 service 层，本文件仅少量本地裁剪时需要）。
