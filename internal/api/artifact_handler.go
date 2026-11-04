package api

// 制品下载（docs/基础功能边界与插件化可行性评估.md R4）。
//
//	GET /api/v1/artifacts/{id}
//
// 鉴权：走全局 Auth 中间件（/api/** 必须携带令牌），本 handler 不再自行鉴权。
// 错误码：非法 id（含路径穿越）→ 400 invalid_argument；CAS 无此内容 → 404 not_found；
// 子系统未接线 → 503 unavailable；读盘失败 → 500 internal。全程不做字符串拼接定路径，
// 因此不存在 panic 面（Recover 中间件仍作为最后兜底）。

import (
	"fmt"
	"io"
	"net/http"
	"strconv"

	"harness-go/internal/apperr"
)

// downloadArtifact 按快照 CAS 引用下载制品二进制流。
//
//	GET /api/v1/artifacts/{id}
func (h *handlers) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	if h.d.Artifacts == nil {
		writeError(w, r, apperr.New(apperr.CodeUnavailable, "制品下载子系统未接线（快照 CAS 缺失）"))
		return
	}
	rc, meta, err := h.d.Artifacts.Open(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", meta.FileName))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if meta.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if _, cErr := io.Copy(w, rc); cErr != nil {
		// 响应头已发出，无法再改写状态码；仅记录日志交由 AccessLog 的字节数观察。
		h.d.Log.Warn("制品流写出中断",
			"artifact", meta.ID, "error", cErr)
	}
}
