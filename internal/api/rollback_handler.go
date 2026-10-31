package api

// 回滚与快照（阶段 G / S10）：消息级 / 轮次级 / 工具级 revert & reapply。

import (
	"net/http"

	"harness-go/internal/apperr"
	"harness-go/internal/service"
)

// revertReq 回滚请求体。
type revertReq = service.RevertRequest

// createRollback 物理回滚（文件写回 + 消息级置灰）并登记回滚事件。
//
//	POST /api/v1/sessions/{sid}/rollback
func (h *handlers) createRollback(w http.ResponseWriter, r *http.Request) {
	if h.d.Rollback == nil {
		writeError(w, r, apperrUnavailableRollback())
		return
	}
	var req revertReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	rb, err := h.d.Rollback.Revert(r.Context(), r.PathValue("sid"), req)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rb)
}

// listRollbacks 会话回滚事件（时间倒序）。
//
//	GET /api/v1/sessions/{sid}/rollbacks
func (h *handlers) listRollbacks(w http.ResponseWriter, r *http.Request) {
	if h.d.Rollback == nil {
		writeError(w, r, apperrUnavailableRollback())
		return
	}
	items, err := h.d.Rollback.List(r.Context(), r.PathValue("sid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// reapplyRollback 撤销一次回滚（恢复 after 快照 + 恢复置灰消息）。
//
//	POST /api/v1/sessions/{sid}/rollbacks/{rid}/reapply
func (h *handlers) reapplyRollback(w http.ResponseWriter, r *http.Request) {
	if h.d.Rollback == nil {
		writeError(w, r, apperrUnavailableRollback())
		return
	}
	rb, err := h.d.Rollback.Reapply(r.Context(), r.PathValue("sid"), r.PathValue("rid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rb)
}

// listSessionSnapshots 会话快照锚点（时间倒序；UI 展示）。
//
//	GET /api/v1/sessions/{sid}/snapshots
func (h *handlers) listSessionSnapshots(w http.ResponseWriter, r *http.Request) {
	if h.d.Rollback == nil {
		writeError(w, r, apperrUnavailableRollback())
		return
	}
	items, err := h.d.Rollback.SnapshotList(r.Context(), r.PathValue("sid"), 200)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func apperrUnavailableRollback() error {
	return apperr.New(apperr.CodeUnavailable, "快照回滚子系统未接线（快照存储缺失）")
}
