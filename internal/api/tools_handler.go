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
