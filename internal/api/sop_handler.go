package api

import (
	"errors"
	"net/http"
	"strings"

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
// session_id 可选：给出时按该会话绑定工作区列出项目级条目，并按会话事实
// （子会话作用域 / 规划模式）渲染真实规则；缺省按主会话 + standard 预览。
// 这是「调教 SOP 的反馈回路」——不跑对话即可确认改动是否正确生效。
func (h *handlers) getSOPEffective(w http.ResponseWriter, r *http.Request) {
	if h.d.SOP == nil {
		writeError(w, r, sopUnavailableErr())
		return
	}
	rc := sop.Context{}
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
		rc.SessionID = sess.ID
	}
	// 基础提示取当前生效的核心 SOP（未装配时为空串，预览仍可返回规则与条目部分）。
	base := h.d.SOP.CoreText()
	writeJSON(w, http.StatusOK, h.d.SOP.Effective(r.Context(), base, wsPath, rc))
}

// ============ 原生 SOP 目录（工程作业程序清单 + 逐条启停） ============

// validatePlanMode 规划模式值域校验（无副作用版）。
//
// 为什么 API 层也要校验一次：PUT /sop/catalog 会**先写开关、后写模式**，
// 若模式非法只能失败在第二步，用户会看到「开关已变、模式没变」的半生效状态。
// 这里先在无副作用阶段拦下，保证整个请求原子（要么都生效，要么都不动）。
func validatePlanMode(mode string) error {
	switch strings.TrimSpace(mode) {
	case sop.PlanModeStandard, sop.PlanModeAlwaysPlan, sop.PlanModeAlwaysProceed:
		return nil
	}
	return apperr.New(apperr.CodeInvalidArgument,
		"plan_mode 取值非法: %q（合法值: standard|always_plan|always_proceed）", mode)
}

// getSOPCatalog 返回原生 SOP 数据字典。
//
//	GET /api/v1/sop/catalog
//
// 响应含每条规则的层级 / 分类 / 作用域 / 是否硬规则 / 说明 / 关闭后果 / 全部变体原文，
// 以及用户当前的 disabled 列表与 plan_mode。前端设置界面按此渲染逐条开关
// （单一真源，不在前端硬编码）。
func (h *handlers) getSOPCatalog(w http.ResponseWriter, r *http.Request) {
	if h.d.SOP == nil {
		writeError(w, r, sopUnavailableErr())
		return
	}
	disabled := []string{}
	mode := sop.PlanModeStandard
	if h.d.Settings != nil {
		if got := h.d.Settings.DisabledSOPs(r.Context()); got != nil {
			disabled = got
		}
		mode = h.d.Settings.PlanMode(r.Context())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":     h.d.SOP.Catalog(),
		"disabled":  disabled,
		"plan_mode": mode,
	})
}

// sopCatalogPutReq 保存启用状态请求体。
type sopCatalogPutReq struct {
	// Disabled 要关闭的规则 ID 列表（空数组 = 全部启用）。
	Disabled []string `json:"disabled"`
	// PlanMode 规划模式（standard | always_plan | always_proceed）；空串 = 不修改。
	//
	// 为什么和开关放在同一个端点：两者共同决定「本轮提示词里出现哪些规则变体」，
	// 分两个端点会让前端保存一次要发两个请求、且中途失败会造成状态不一致。
	PlanMode string `json:"plan_mode"`
}

// putSOPCatalog 保存逐条启停状态与规划模式（下一轮对话立即生效）。
//
//	PUT /api/v1/sop/catalog
//
// 校验口径在 service.SettingsService.SetDisabledSOPs / SetPlanMode（与 PATCH /settings 同一套）：
// 未知 ID / 重复 ID / 空 ID / 硬规则 ID 一律 400（绝不静默忽略）。
func (h *handlers) putSOPCatalog(w http.ResponseWriter, r *http.Request) {
	if h.d.SOP == nil {
		writeError(w, r, sopUnavailableErr())
		return
	}
	if h.d.Settings == nil {
		writeError(w, r, apperr.New(apperr.CodeUnavailable, "设置子系统未接入"))
		return
	}
	var req sopCatalogPutReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	// 先校验模式（无副作用），全部通过后再落库，避免「开关写进去了、模式又被拒」的半生效。
	if strings.TrimSpace(req.PlanMode) != "" {
		if err := validatePlanMode(req.PlanMode); err != nil {
			writeError(w, r, err)
			return
		}
	}
	saved, err := h.d.Settings.SetDisabledSOPs(r.Context(), req.Disabled)
	if err != nil {
		writeError(w, r, err)
		return
	}
	mode := h.d.Settings.PlanMode(r.Context())
	if strings.TrimSpace(req.PlanMode) != "" {
		got, mErr := h.d.Settings.SetPlanMode(r.Context(), req.PlanMode)
		if mErr != nil {
			writeError(w, r, mErr)
			return
		}
		mode = got
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":     h.d.SOP.Catalog(),
		"disabled":  saved,
		"plan_mode": mode,
	})
}
