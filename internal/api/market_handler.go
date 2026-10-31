package api

import (
	"net/http"

	"harness-go/internal/apperr"
	"harness-go/internal/service"
)

// mcpUnavailableErr 构造「外部插件子系统未接入」错误。
func mcpUnavailableErr() error {
	return apperr.New(apperr.CodeUnavailable, "外部插件子系统未接入（mcp.enabled=false 或装配缺失）")
}

// ============ MCP Server 管理（阶段 E / docs/MCP接入设计.md §9） ============

// listMCPServers 返回全部 Server（配置 + 运行态合并；密钥脱敏）。
//
//	GET /api/v1/mcp/servers
func (h *handlers) listMCPServers(w http.ResponseWriter, r *http.Request) {
	if h.d.MCP == nil {
		writeError(w, r, mcpUnavailableErr())
		return
	}
	items, err := h.d.MCP.List(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// getMCPServer 单个 Server 视图。
//
//	GET /api/v1/mcp/servers/{id}
func (h *handlers) getMCPServer(w http.ResponseWriter, r *http.Request) {
	if h.d.MCP == nil {
		writeError(w, r, mcpUnavailableErr())
		return
	}
	v, err := h.d.MCP.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// mcpUpsertReq 创建/更新请求体。
type mcpUpsertReq = service.MCPCreateInput

// createMCPServer 新增（可选 start=true 立即启动）。
//
//	POST /api/v1/mcp/servers?start=true
func (h *handlers) createMCPServer(w http.ResponseWriter, r *http.Request) {
	if h.d.MCP == nil {
		writeError(w, r, mcpUnavailableErr())
		return
	}
	var req mcpUpsertReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	v, err := h.d.MCP.Create(r.Context(), req, r.URL.Query().Get("start") == "true")
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// updateMCPServer 全量更新（变更后需显式 reload 生效）。
//
//	PUT /api/v1/mcp/servers/{id}
func (h *handlers) updateMCPServer(w http.ResponseWriter, r *http.Request) {
	if h.d.MCP == nil {
		writeError(w, r, mcpUnavailableErr())
		return
	}
	var req mcpUpsertReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	v, err := h.d.MCP.Update(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// deleteMCPServer 删除（先注销工具再关连接）。
//
//	DELETE /api/v1/mcp/servers/{id}
func (h *handlers) deleteMCPServer(w http.ResponseWriter, r *http.Request) {
	if h.d.MCP == nil {
		writeError(w, r, mcpUnavailableErr())
		return
	}
	if err := h.d.MCP.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mcpTestReq 联通测试请求体（配置可未落库）。
type mcpTestReq = service.MCPCreateInput

// testMCPServer 联通测试：建连 → 握手 → tools/list → 清理；可读失败原因。
//
//	POST /api/v1/mcp/test
func (h *handlers) testMCPServer(w http.ResponseWriter, r *http.Request) {
	if h.d.MCP == nil {
		writeError(w, r, mcpUnavailableErr())
		return
	}
	var req mcpTestReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	res, err := h.d.MCP.Test(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// reloadMCPServer 热重载（原子替换，失败保留旧连接）。
//
//	POST /api/v1/mcp/servers/{id}/reload
func (h *handlers) reloadMCPServer(w http.ResponseWriter, r *http.Request) {
	if h.d.MCP == nil {
		writeError(w, r, mcpUnavailableErr())
		return
	}
	if err := h.d.MCP.Reload(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// enableMCPServer / disableMCPServer 启停。
//
//	POST /api/v1/mcp/servers/{id}/enable | disable
func (h *handlers) enableMCPServer(w http.ResponseWriter, r *http.Request) {
	if h.d.MCP == nil {
		writeError(w, r, mcpUnavailableErr())
		return
	}
	if err := h.d.MCP.Enable(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *handlers) disableMCPServer(w http.ResponseWriter, r *http.Request) {
	if h.d.MCP == nil {
		writeError(w, r, mcpUnavailableErr())
		return
	}
	if err := h.d.MCP.Disable(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// mcpServerHealth 健康详情。
//
//	GET /api/v1/mcp/servers/{id}/health
func (h *handlers) mcpServerHealth(w http.ResponseWriter, r *http.Request) {
	if h.d.MCP == nil {
		writeError(w, r, mcpUnavailableErr())
		return
	}
	st, err := h.d.MCP.Health(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// ============ 本地工具包（pkg.zip） ============

// listPackages 全部已安装工具包。
//
//	GET /api/v1/packages
func (h *handlers) listPackages(w http.ResponseWriter, r *http.Request) {
	if h.d.Packages == nil {
		writeError(w, r, mcpUnavailableErr())
		return
	}
	items, err := h.d.Packages.List(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// installPackage 安装本地工具包 zip（信任策略在 service 层硬约束）。
//
//	POST /api/v1/packages/install  {"path": "<zip 路径>"}
//
// 设计说明：一期接受「服务端本地路径」（本地单机一等公民，包由用户/私有 registry
// 预先放置）；HTTP 上传与私有 registry 拉取属后续增强，不改变安装内核。
type installPackageReq struct {
	Path string `json:"path"`
}

func (h *handlers) installPackage(w http.ResponseWriter, r *http.Request) {
	if h.d.Packages == nil {
		writeError(w, r, mcpUnavailableErr())
		return
	}
	var req installPackageReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if req.Path == "" {
		writeError(w, r, invalidArgErr("path 不能为空"))
		return
	}
	res, err := h.d.Packages.Install(r.Context(), req.Path)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// uninstallPackage 卸载（注销工具 + 清目录 + 删行）。
//
//	DELETE /api/v1/packages/{id}
func (h *handlers) uninstallPackage(w http.ResponseWriter, r *http.Request) {
	if h.d.Packages == nil {
		writeError(w, r, mcpUnavailableErr())
		return
	}
	if err := h.d.Packages.Uninstall(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// rollbackPackage 回滚到上一版本。
//
//	POST /api/v1/packages/{id}/rollback
func (h *handlers) rollbackPackage(w http.ResponseWriter, r *http.Request) {
	if h.d.Packages == nil {
		writeError(w, r, mcpUnavailableErr())
		return
	}
	v, err := h.d.Packages.Rollback(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// setPackageEnabled 启停（禁用保留安装）。
//
//	POST /api/v1/packages/{id}/enable | disable
func (h *handlers) enablePackage(w http.ResponseWriter, r *http.Request) {
	if h.d.Packages == nil {
		writeError(w, r, mcpUnavailableErr())
		return
	}
	if err := h.d.Packages.SetEnabled(r.Context(), r.PathValue("id"), true); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *handlers) disablePackage(w http.ResponseWriter, r *http.Request) {
	if h.d.Packages == nil {
		writeError(w, r, mcpUnavailableErr())
		return
	}
	if err := h.d.Packages.SetEnabled(r.Context(), r.PathValue("id"), false); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
