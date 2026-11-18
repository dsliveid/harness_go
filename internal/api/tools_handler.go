package api

import (
	"net/http"

	"harness-go/internal/apperr"
)

// toolUnavailableErr 构造「工具子系统未接入」错误（服务端装配缺失）。
func toolUnavailableErr() error {
	return apperr.New(apperr.CodeToolUnavailable, "工具子系统未接入")
}

// invalidArgErr 构造参数错误（400）。
func invalidArgErr(format string, args ...any) error {
	return apperr.New(apperr.CodeInvalidArgument, format, args...)
}

// ============ 工具（A-10 协议层可观测端点） ============

// listTools 返回工具注册表快照：tier / default_tier / cost_tokens / health / 软上限告警。
//
//	GET /api/v1/tools
func (h *handlers) listTools(w http.ResponseWriter, r *http.Request) {
	if h.d.Tools == nil {
		writeError(w, r, toolUnavailableErr())
		return
	}
	writeJSON(w, http.StatusOK, h.d.Tools.List(r.Context()))
}

// toolsInjection 返回本轮注入视图与 token 预算明细（core 全量 / deferred 索引 / on_demand 零注入）。
//
//	GET /api/v1/tools/injection
func (h *handlers) toolsInjection(w http.ResponseWriter, r *http.Request) {
	if h.d.Tools == nil {
		writeError(w, r, toolUnavailableErr())
		return
	}
	writeJSON(w, http.StatusOK, h.d.Tools.Injection(r.Context()))
}

// tierPatchReq 档位覆盖请求体。
type tierPatchReq struct {
	Tiers map[string]string `json:"tiers"`
}

// patchToolTiers 调整工具档位（写入主库 settings.tools.tiers，**下一轮对话生效**）。
//
//	PATCH /api/v1/tools/tiers
func (h *handlers) patchToolTiers(w http.ResponseWriter, r *http.Request) {
	if h.d.Tools == nil {
		writeError(w, r, toolUnavailableErr())
		return
	}
	var req tierPatchReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if len(req.Tiers) == 0 {
		writeError(w, r, invalidArgErr("tiers 不能为空"))
		return
	}
	snap, err := h.d.Tools.SetTiers(r.Context(), req.Tiers)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// ============ 系统内置工具启停（服务端自身能力开关） ============
//
// 为什么内置工具**不提供安装/卸载**：它们随程序发布，编译期即注册，
// 没有独立的制品可装卸；服务端允许的能力管理只有「启用/停用」一档。
// 客户端（PC / 移动）的内置工具才有「安装到本机」的语义（见 pluginStore）。

// enableTool / disableTool 启用或停用单个内置工具（写入主库 settings.tools.disabled）。
//
//	POST /api/v1/tools/{name}/enable | /disable
//
// 停用后该工具既不进入注入集合（模型看不到）也无法被调用（Resolve 报 ErrUnavailable），
// 下一轮对话生效，无需重启。
func (h *handlers) enableTool(w http.ResponseWriter, r *http.Request) {
	h.setToolEnabled(w, r, true)
}

func (h *handlers) disableTool(w http.ResponseWriter, r *http.Request) {
	h.setToolEnabled(w, r, false)
}

func (h *handlers) setToolEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	if h.d.Tools == nil {
		writeError(w, r, toolUnavailableErr())
		return
	}
	snap, err := h.d.Tools.SetToolEnabled(r.Context(), r.PathValue("name"), enabled)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}
