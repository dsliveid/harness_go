package api

import (
	"net/http"

	"harness-go/internal/apperr"
	"harness-go/internal/service"
)

// 本文件：工作空间域 HTTP 端点（Workspace = 执行面 / 客户端接入点，能力天花板的实体载体）。
//
// 设计要点：
//   - 传输层只做 JSON 形状转换与参数校验，业务裁决全在 service.WorkspaceService；
//   - Workspaces 依赖为 nil 时（未装配工作空间域）统一返回 503 unavailable，
//     与 Projects/Webhooks 等可选子系统的既有约定一致；
//   - **kind 不可修改**：PATCH 里只要传了与库中不同的 kind 直接 400（改 kind = 提权）。
//     docs/工作空间模型与三端能力收口协议.md §3

// workspacesUnavailableErr 工作空间域未装配时的统一错误。
func workspacesUnavailableErr() error {
	return apperr.New(apperr.CodeUnavailable, "工作空间域未启用")
}

// ============ 工作空间 CRUD ============

type createWorkspaceReq struct {
	Name string `json:"name"`
	// Kind 必填：server | desktop | mobile | browser（取值与 web/src/types.ts 逐字一致）。
	Kind string `json:"kind"`
	// DeviceID 可选：空 = 服务端内置空间语义；非空必须对应一台已配对设备。
	DeviceID string `json:"device_id"`
	RootPath string `json:"root_path"`
}

// listWorkspaces 工作空间列表（服务端内置空间排最前，其余按创建时间升序）。
//
//	GET /api/v1/workspaces
func (h *handlers) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	if h.d.Workspaces == nil {
		writeError(w, r, workspacesUnavailableErr())
		return
	}
	items, err := h.d.Workspaces.List(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// createWorkspace 新建工作空间。
//
//	POST /api/v1/workspaces
func (h *handlers) createWorkspace(w http.ResponseWriter, r *http.Request) {
	if h.d.Workspaces == nil {
		writeError(w, r, workspacesUnavailableErr())
		return
	}
	var req createWorkspaceReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	v, err := h.d.Workspaces.Create(r.Context(), service.CreateWorkspaceInput{
		Name:     req.Name,
		Kind:     req.Kind,
		DeviceID: req.DeviceID,
		RootPath: req.RootPath,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// getWorkspace 取单个工作空间。
//
//	GET /api/v1/workspaces/{wid}
func (h *handlers) getWorkspace(w http.ResponseWriter, r *http.Request) {
	if h.d.Workspaces == nil {
		writeError(w, r, workspacesUnavailableErr())
		return
	}
	v, err := h.d.Workspaces.Get(r.Context(), r.PathValue("wid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// updateWorkspaceReq 全部字段用指针承接，实现真正的 PATCH 语义（缺省 = 不改）。
//
// 🚨 Kind 只接受与原值相同的值；不同即 400（能力天花板不可被 PATCH 抬高）。
type updateWorkspaceReq struct {
	Name     *string `json:"name"`
	Kind     *string `json:"kind"`
	DeviceID *string `json:"device_id"`
	RootPath *string `json:"root_path"`
}

// updateWorkspace 更新工作空间（kind 不可修改）。
//
//	PATCH /api/v1/workspaces/{wid}
func (h *handlers) updateWorkspace(w http.ResponseWriter, r *http.Request) {
	if h.d.Workspaces == nil {
		writeError(w, r, workspacesUnavailableErr())
		return
	}
	var req updateWorkspaceReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	v, err := h.d.Workspaces.Update(r.Context(), r.PathValue("wid"), service.UpdateWorkspaceInput{
		Name:     req.Name,
		Kind:     req.Kind,
		DeviceID: req.DeviceID,
		RootPath: req.RootPath,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// deleteWorkspace 删除工作空间（仍被会话/项目引用时 409，不级联）。
//
//	DELETE /api/v1/workspaces/{wid}
func (h *handlers) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if h.d.Workspaces == nil {
		writeError(w, r, workspacesUnavailableErr())
		return
	}
	if err := h.d.Workspaces.Delete(r.Context(), r.PathValue("wid")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
