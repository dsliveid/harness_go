package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"harness-go/internal/model"
)

// ============ 审批（B-3） ============

type decideApprovalReq struct {
	Decision string `json:"decision"` // allow_once | allow_session | deny
	Reason   string `json:"reason"`
}

// decideApproval 提交审批决策：POST /api/v1/sessions/{sid}/runs/{rid}/approvals/{aid}/decide
func (h *handlers) decideApproval(w http.ResponseWriter, r *http.Request) {
	var req decideApprovalReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	err := h.d.Runs.DecideApproval(r.Context(),
		r.PathValue("sid"), r.PathValue("rid"), r.PathValue("aid"), req.Decision, req.Reason)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"approval_id": r.PathValue("aid"),
		"decision":    req.Decision,
		"status":      "accepted",
	})
}

// ============ 厂商（B-4） ============

type providerReq struct {
	Name    string   `json:"name"`
	BaseURL string   `json:"base_url"`
	APIKey  string   `json:"api_key"`
	Models  []string `json:"models"`
	Enabled *bool    `json:"enabled"`
}

func (req providerReq) toModel() model.Provider {
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	return model.Provider{
		Name:    req.Name,
		BaseURL: req.BaseURL,
		APIKey:  req.APIKey,
		Models:  req.Models,
		Enabled: enabled,
	}
}

func (h *handlers) listProviders(w http.ResponseWriter, r *http.Request) {
	items, err := h.d.Providers.List(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *handlers) createProvider(w http.ResponseWriter, r *http.Request) {
	var req providerReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	p, err := h.d.Providers.Create(r.Context(), req.toModel())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *handlers) updateProvider(w http.ResponseWriter, r *http.Request) {
	var req providerReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	p, err := h.d.Providers.Update(r.Context(), r.PathValue("pid"), req.toModel())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *handlers) deleteProvider(w http.ResponseWriter, r *http.Request) {
	if err := h.d.Providers.Delete(r.Context(), r.PathValue("pid")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type providerTestReq struct {
	Model string `json:"model"`
}

func (h *handlers) testProvider(w http.ResponseWriter, r *http.Request) {
	var req providerTestReq
	// 允许空 body（前端不给 model 时用厂商首个模型）
	if err := decodeJSON(r, &req); err != nil {
		// 只有「请求体为空/格式不完整」刻意忽略（Body 为 EOF），其余照常报错
		if !isBodyEmpty(err) {
			writeError(w, r, err)
			return
		}
		req.Model = ""
	}
	res, err := h.d.Providers.Test(r.Context(), r.PathValue("pid"), req.Model)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ============ 配置（B-4） ============

func (h *handlers) getSettings(w http.ResponseWriter, r *http.Request) {
	items, err := h.d.Settings.Get(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *handlers) patchSettings(w http.ResponseWriter, r *http.Request) {
	var patch map[string]json.RawMessage
	if err := decodeJSON(r, &patch); err != nil {
		writeError(w, r, err)
		return
	}
	items, err := h.d.Settings.Patch(r.Context(), patch)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ============ 统计与日志（B-4） ============
func (h *handlers) stats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.d.Maintenance.Stats(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (h *handlers) logs(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 200)
	level := r.URL.Query().Get("level")
	lines, err := h.d.Logs.Tail(limit, level)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": lines, "limit": limit})
}

// isBodyEmpty 判断解析失败是否仅仅因为请求体为空（EOF）。
//
// 用于「可选 body」端点（如厂商连通测试）：空 body 用默认值，格式错误仍报 400。
func isBodyEmpty(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
