package api

import (
	"net/http"

	"harness-go/internal/apperr"
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
// DefaultAccessMode 用指针区分「未传」与「显式设置」——避免前端只改名字时
// 把默认权限意外重置成 confirm（这属于会静默放大/缩小权限的危险默认值）。
type updateProjectReq struct {
	Name *string `json:"name"`
	// DefaultAccessMode 非 nil 时更新项目默认控制权限（仅影响此后新建的会话）。
	DefaultAccessMode *string `json:"default_access_mode"`
	Pinned            *bool   `json:"pinned"`
}

// updateProject 更新项目（名称 / 路径 / 默认控制权限 / 置顶）。
//
//	PATCH /api/v1/projects/{pid}
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
	pid := r.PathValue("pid")
	if req.DefaultAccessMode != nil {
		if err := h.d.Projects.SetDefaultAccessMode(r.Context(), pid, *req.DefaultAccessMode); err != nil {
			writeError(w, r, err)
			return
		}
	}
	// 名称/置顶属于「整体覆盖」语义，需基于当前值合并后才落库，
	// 否则只传 pinned 会把 name 清空。
	// 注意：项目路径（path）创建后不再变更——它是会话工作区的配置源，
	// 改目录应新建项目而非原地改，避免历史会话产生歧义。
	if req.Name != nil || req.Pinned != nil {
		cur, err := h.d.Projects.Get(r.Context(), pid)
		if err != nil {
			writeError(w, r, err)
			return
		}
		name, pinned := cur.Name, cur.Pinned
		if req.Name != nil {
			name = *req.Name
		}
		if req.Pinned != nil {
			pinned = *req.Pinned
		}
		// defaultAccessMode 传空串 = 保留原值（已在上面单独处理过显式修改）。
		if err := h.d.Projects.Update(r.Context(), pid, name, "", pinned); err != nil {
			writeError(w, r, err)
			return
		}
	}
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
// _ 保持 strings 引用（部分校验在 service 层，本文件仅少量本地裁剪时需要）。
