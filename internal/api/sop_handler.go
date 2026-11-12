package api

import (
	"errors"
	"net/http"

	"harness-go/internal/apperr"
	"harness-go/internal/sop"
)

// sopUnavailableErr 构造「SOP 子系统未接入」错误（服务端装配缺失）。
func sopUnavailableErr() error {
	return apperr.New(apperr.CodeUnavailable, "SOP 子系统未接入")
}

// ============ 核心 SOP（agent-core）：出厂内置 + 运行时覆盖 ============

// getSOPCore 返回核心 SOP 生效快照。
//
//	GET /api/v1/sop/core
//
// 响应含 content / source(builtin|custom) / tokens / budget /
// builtin_content / overridden。前端「系统内置」页签渲染 builtin_content，
// 因此**切换页签不需要任何读写请求**，从根上杜绝误删用户自定义内容。
func (h *handlers) getSOPCore(w http.ResponseWriter, r *http.Request) {
	if h.d.SOP == nil {
		writeError(w, r, sopUnavailableErr())
		return
	}
	writeJSON(w, http.StatusOK, h.d.SOP.Core(r.Context()))
}

// sopCorePutReq 保存核心 SOP 请求体。
type sopCorePutReq struct {
	Content string `json:"content"`
}

// putSOPCore 保存自定义核心 SOP（首次保存即完成「内置 → 自定义」fork）。
//
//	PUT /api/v1/sop/core
//
// 超预算 → 400（显式拒绝，绝不截断，磁盘保持原值）。
func (h *handlers) putSOPCore(w http.ResponseWriter, r *http.Request) {
	if h.d.SOP == nil {
		writeError(w, r, sopUnavailableErr())
		return
	}
	var req sopCorePutReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	core, err := h.d.SOP.SaveCore(r.Context(), req.Content)
	if err != nil {
		if errors.Is(err, sop.ErrBudgetExceeded) {
			writeError(w, r, apperr.New(apperr.CodeInvalidArgument, "%s", err.Error()))
			return
		}
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, core)
}

// deleteSOPCore 删除自定义覆盖层，回落出厂内置（完整 undo）。
//
//	DELETE /api/v1/sop/core
func (h *handlers) deleteSOPCore(w http.ResponseWriter, r *http.Request) {
	if h.d.SOP == nil {
		writeError(w, r, sopUnavailableErr())
		return
	}
	core, err := h.d.SOP.ResetCore(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, core)
}

// getSOPEffective 返回生效预览：本轮真正注入模型的 SOP 组合与 token 合计。
//
//	GET /api/v1/sop/effective?session_id=xxx
//
// session_id 可选：给出时按该会话绑定工作区列出项目级条目，缺省则只列核心 SOP。
// 这是「调教 SOP 的反馈回路」——不跑对话即可确认改动是否正确生效。
func (h *handlers) getSOPEffective(w http.ResponseWriter, r *http.Request) {
	if h.d.SOP == nil {
		writeError(w, r, sopUnavailableErr())
		return
	}
	wsPath := ""
	if sid := r.URL.Query().Get("session_id"); sid != "" {
		if h.d.Sessions == nil {
			writeError(w, r, apperr.New(apperr.CodeUnavailable, "会话子系统未接入"))
			return
		}
		sess, err := h.d.Sessions.Get(r.Context(), sid)
		if err != nil {
			writeError(w, r, err)
			return
		}
		wsPath = sess.ProjectDir
	}
	// 基础提示取当前生效的核心 SOP（未装配时为空串，预览仍可返回条目部分）。
	base := h.d.SOP.CoreText()
	writeJSON(w, http.StatusOK, h.d.SOP.Effective(r.Context(), base, wsPath))
}
