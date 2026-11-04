package api

// 会话导出 / 全量备份 / 恢复（docs/基础功能边界与插件化可行性评估.md R2）。
//
//	GET  /api/v1/sessions/{sid}/export?format=zip   会话导出（含消息、tool_events、快照引用）
//	GET  /api/v1/data/backup                        全量 data/ 归档为 zip 流（owner 专用）
//	POST /api/v1/data/restore                       接收 zip 恢复（校验后落暂存目录）
//
// 鉴权：三条路由均在 /api/** 前缀下，由全局 Auth 中间件强制令牌；
// backup / restore 额外校验账号角色为 owner（403 forbidden）。
//
// ⚠️ 诚实边界（数据权限）：文档 R2 提到会话导出「走数据权限 session_export」、
// R4 提到制品下载「走审计 + 数据权限 fs_outside/session_export」。当前代码库
// **不存在**数据权限/审计审批子系统（internal 下无对应包，grep 无 session_export /
// fs_outside 命中），因此本实现在权限维度只能做到「全局鉴权 + 会话存在性校验」，
// 不伪造审批流。详见交付报告「遗留问题与诚实边界」。

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"harness-go/internal/apperr"
)

// exportSession GET /api/v1/sessions/{sid}/export?format=zip
//
// 复用内核 BundleService（与 session-bundle 插件的 export_session 同一执行体）：
// 包内含 manifest.json / messages.jsonl / runs.json / tool_events.jsonl / blobs/**。
// 导出包落 data/sessions/<sid>/bundle/（内核既定位置，可被 bundle List 枚举），
// 随后按流回传。
func (h *handlers) exportSession(w http.ResponseWriter, r *http.Request) {
	if h.d.Bundles == nil {
		writeError(w, r, apperr.New(apperr.CodeUnavailable, "会话导出子系统未接线（导出包服务缺失）"))
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "zip"
	}
	if format != "zip" {
		writeError(w, r, apperr.New(apperr.CodeInvalidArgument,
			"不支持的导出格式 %q：仅支持 format=zip", format))
		return
	}
	sid := strings.TrimSpace(r.PathValue("sid"))
	if sid == "" {
		writeError(w, r, apperr.New(apperr.CodeInvalidArgument, "会话 id 不能为空"))
		return
	}
	info, err := h.d.Bundles.Export(r.Context(), sid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	streamZipFile(w, r, info.Path, fmt.Sprintf("session-%s-export.zip", sid))
}

// backupData GET /api/v1/data/backup（owner 专用）
//
// 全量归档 data/ 为 zip 流：先落 data/tmp 临时文件（与数据同盘），流式回传后删除。
func (h *handlers) backupData(w http.ResponseWriter, r *http.Request) {
	if h.d.Backups == nil {
		writeError(w, r, apperr.New(apperr.CodeUnavailable, "备份子系统未接线（数据目录未知）"))
		return
	}
	if err := h.requireOwner(r); err != nil {
		writeError(w, r, err)
		return
	}
	tmpPath, err := h.d.Backups.TempZipPath("backup")
	if err != nil {
		writeError(w, r, apperr.Wrap(err, apperr.CodeInternal, "创建备份临时文件失败"))
		return
	}
	defer h.d.Backups.RemoveArtifact(tmpPath)

	if _, err := h.d.Backups.CreateBackup(r.Context(), tmpPath); err != nil {
		writeError(w, r, err)
		return
	}
	streamZipFile(w, r, tmpPath, fmt.Sprintf("harness-backup-%d.zip", nowUnixMilli()))
}

// restoreData POST /api/v1/data/restore（owner 专用）
//
// 请求体为备份包本体（Content-Type: application/zip，原始字节流，非 JSON 包裹）。
// 安全边界：先写 data/tmp 临时文件 → 全量校验（条目名穿越/符号链接/解压体积/清单 schema）
// → 解压到 data/restore-staging-<ts>。**绝不覆盖运行中的 data/**，实际替换需停机操作
// （响应体明确返回 staging_path 与 requires_restart）。
func (h *handlers) restoreData(w http.ResponseWriter, r *http.Request) {
	if h.d.Backups == nil {
		writeError(w, r, apperr.New(apperr.CodeUnavailable, "备份子系统未接线（数据目录未知）"))
		return
	}
	if err := h.requireOwner(r); err != nil {
		writeError(w, r, err)
		return
	}
	if ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))); ct != "" &&
		!strings.HasPrefix(ct, "application/zip") &&
		!strings.HasPrefix(ct, "application/octet-stream") {
		writeError(w, r, apperr.New(apperr.CodeInvalidArgument,
			"恢复请求体必须是备份包原始 zip 字节流（Content-Type: application/zip）"))
		return
	}
	tmpPath, err := h.d.Backups.TempZipPath("restore")
	if err != nil {
		writeError(w, r, apperr.Wrap(err, apperr.CodeInternal, "创建恢复临时文件失败"))
		return
	}
	defer h.d.Backups.RemoveArtifact(tmpPath)

	// 上传体积硬上限：防止无穷流写满磁盘（解压体积另有 service 层上限）。
	limited := io.LimitReader(r.Body, maxRestoreUploadBytes+1)
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		writeError(w, r, apperr.Wrap(err, apperr.CodeInternal, "创建恢复临时文件失败"))
		return
	}
	n, cpErr := io.Copy(f, limited)
	closeErr := f.Close()
	if cpErr != nil {
		writeError(w, r, apperr.Wrap(cpErr, apperr.CodeInternal, "读取上传的备份包失败"))
		return
	}
	if closeErr != nil {
		writeError(w, r, apperr.Wrap(closeErr, apperr.CodeInternal, "写入恢复临时文件失败"))
		return
	}
	if n == 0 {
		writeError(w, r, apperr.New(apperr.CodeInvalidArgument, "恢复请求体为空：需要备份包 zip 字节流"))
		return
	}
	if n > maxRestoreUploadBytes {
		writeError(w, r, apperr.New(apperr.CodeInvalidArgument,
			"备份包超过上传上限 %d 字节", int64(maxRestoreUploadBytes)))
		return
	}

	res, err := h.d.Backups.Restore(r.Context(), tmpPath)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// maxRestoreUploadBytes 恢复上传体积硬上限（与 service 层解压上限同量级）。
const maxRestoreUploadBytes int64 = 2 << 30

// ownerRole 主库 users.role 的 owner 取值（与 auth.EnsureBootstrapUser 写入值一致）。
const ownerRole = "owner"

// requireOwner 校验当前账号角色为 owner（无 Auth 子系统时视为不满足，返回 unavailable）。
func (h *handlers) requireOwner(r *http.Request) error {
	if h.d.Auth == nil {
		return apperr.New(apperr.CodeUnavailable, "鉴权子系统未接线（无法校验 owner 权限）")
	}
	uid := UserID(r.Context())
	if uid == "" {
		return apperr.New(apperr.CodeUnauthorized, "缺少访问令牌")
	}
	user, err := h.d.Auth.Me(r.Context(), uid)
	if err != nil {
		return err
	}
	if user == nil || user.Role != ownerRole {
		return apperr.New(apperr.CodeForbidden, "该操作仅限 owner 账号")
	}
	return nil
}

// streamZipFile 以附件形式流式回传本地 zip 文件。
func streamZipFile(w http.ResponseWriter, r *http.Request, path, fileName string) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, r, apperr.Wrap(err, apperr.CodeNotFound, "归档文件不存在"))
			return
		}
		writeError(w, r, apperr.Wrap(err, apperr.CodeInternal, "打开归档文件失败"))
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		writeError(w, r, apperr.Wrap(err, apperr.CodeInternal, "读取归档文件信息失败"))
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileName))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", fi.Size()))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// nowUnixMilli 当前毫秒时间戳（备份文件名用）。
func nowUnixMilli() int64 { return time.Now().UnixMilli() }
