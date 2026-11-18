package api

import (
	"net/http"

	"harness-go/internal/apperr"
	"harness-go/internal/service"
)

// skillUnavailableErr 构造「技能子系统未接入」错误。
func skillUnavailableErr() error {
	return apperr.New(apperr.CodeUnavailable, "技能子系统未接入（装配缺失）")
}

// ============ 技能库（.harness/skills 唯一真源的服务端 HTTP 通道） ============
//
// 为什么需要这组端点：技能的真源一直是 `.harness/skills/<name>/SKILL.md`，
// Agent 侧的 skill_list / skill_read / skill_save 工具早已在读写它；
// 但 Web 端从未有通道读取，导致「Agent 沉淀的技能在界面里永远是空的」。
// 这里只是把同一份真源暴露成 HTTP，不新建任何目录或第二份数据。

// listSkills 列出技能库全部技能。
//
//	GET /api/v1/skills
func (h *handlers) listSkills(w http.ResponseWriter, r *http.Request) {
	if h.d.Skills == nil {
		writeError(w, r, skillUnavailableErr())
		return
	}
	items, err := h.d.Skills.List(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	// root 一并回传：界面上必须能说清「技能落在哪个目录」（进程根 vs 项目根差异见 service 注释）
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"root":  h.d.Skills.Root(),
		"dir":   service.SkillsDirRel,
	})
}

// getSkill 读取单个技能（含 SKILL.md 全文，供界面预览）。
//
//	GET /api/v1/skills/{name}
func (h *handlers) getSkill(w http.ResponseWriter, r *http.Request) {
	if h.d.Skills == nil {
		writeError(w, r, skillUnavailableErr())
		return
	}
	view, content, err := h.d.Skills.Read(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"view": view, "content": content})
}

// saveSkillReq 保存技能请求体（与 skill_save 工具入参同形）。
type saveSkillReq = service.SaveInput

// saveSkill 沉淀/覆盖技能（写 .harness/skills/<name>/SKILL.md）。
//
//	POST /api/v1/skills
func (h *handlers) saveSkill(w http.ResponseWriter, r *http.Request) {
	if h.d.Skills == nil {
		writeError(w, r, skillUnavailableErr())
		return
	}
	var req saveSkillReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	view, err := h.d.Skills.Save(r.Context(), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// deleteSkill 卸载技能（删除整个技能目录，含附属脚本）。
//
//	DELETE /api/v1/skills/{name}
func (h *handlers) deleteSkill(w http.ResponseWriter, r *http.Request) {
	if h.d.Skills == nil {
		writeError(w, r, skillUnavailableErr())
		return
	}
	if err := h.d.Skills.Delete(r.Context(), r.PathValue("name")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
