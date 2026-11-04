package service

// BackupService 全量备份 / 恢复（docs/基础功能边界与插件化可行性评估.md R2）。
//
// 包形态（backup.zip，条目路径 = 相对 dataDir 的斜杠路径）：
//
//	backup-manifest.json   {schema, created_at, entries, bytes, skipped}
//	main.db               主库（用户/会话摘要/厂商/设置/设备/检索/市场）
//	sessions/<sid>/session.db   会话库
//	sessions/<sid>/blobs/<xx>/<hash>  会话级 CAS
//	snapshots/blobs/<xx>/<hash>       全局快照 CAS
//	<其余数据文件>
//
// ⚠️ 备份一致性边界（诚实声明，勿当事务快照用）：
//   - 本实现是**文件级**归档：逐个文件读盘打包，不做 SQLite 事务快照，也不做
//     WAL checkpoint。因此并发写入窗口内的备份可能出现 main.db 与其 -wal 不配对。
//   - 因此归档时**跳过** `-wal` / `-shm` / `data/tmp/**` / `*.part` / 恢复暂存目录：
//     跳过 -wal 意味着「已提交但尚未 checkpoint 的事务」不在包内，语义是
//     「最后一次 checkpoint 的落盘状态」，这是显式取舍而非 bug。
//   - 建议在低写入窗口执行备份（前端数据面板入口即人工触发）。
//
// ⚠️ 恢复安全边界（严禁覆盖生产数据目录）：
//   - 解压到 dataDir 下的**独立暂存目录**（restore-staging-<ts>），全程不触碰
//     运行中的 data/ 内容；进程持有 SQLite 句柄与 WAL，在线原子替换 dataDir
//     在技术上是不可靠的（Half-written 主库 = 服务不可启动）。
//   - 因此本服务只做「校验 + 暂存 + 明确告知」，返回 staging_path + requires_restart，
//     真正启用需停机后由运维按路径切换（这是显式安全边界，不是功能缺失掩饰）。
//   - 校验项：条目名必须是相对路径、不得含 `..` / 盘符 / 反斜杠 / 绝对路径、
//     不得是符号链接或目录外软链、单条目与总解压体积有上限、必须含 backup-manifest.json
//     且 schema 不高于当前支持、暂存目录中必须存在 main.db。

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"harness-go/internal/apperr"
)

const (
	// backupSchema 备份包格式版本（只允许向后兼容新增字段）。
	backupSchema = 1
	// backupManifestName 备份包清单文件名（与会话导出包的 manifest.json 区分）。
	backupManifestName = "backup-manifest.json"
	// BackupManifestName 对外暴露的清单条目名（传输层测试与外部校验脚本共用）。
	BackupManifestName = backupManifestName
	// maxBackupEntries 备份包条目数上限（防解压炸弹：条目数维度）。
	maxBackupEntries = 50000
	// maxRestoreBytes 解压后总体积上限（防解压炸弹：体积维度）。
	maxRestoreBytes int64 = 2 << 30 // 2 GiB
	// maxRestoreEntryBytes 单条目解压体积上限。
	maxRestoreEntryBytes int64 = 512 << 20 // 512 MiB
)

// BackupServiceOptions 备份服务依赖。
type BackupServiceOptions struct {
	// DataDir 数据目录（归档根 + 暂存目录落点）。
	DataDir string
	// Now 时钟（测试注入）。
	Now func() time.Time
}

// BackupService 全量备份与恢复。
type BackupService struct {
	dataDir string
	tmpDir  string
	now     func() time.Time
}

// NewBackupService 创建备份服务。
func NewBackupService(opts BackupServiceOptions) *BackupService {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &BackupService{
		dataDir: opts.DataDir,
		tmpDir:  filepath.Join(opts.DataDir, "tmp"),
		now:     now,
	}
}

// backupManifest 备份包清单。
type backupManifest struct {
	Schema    int      `json:"schema"`
	CreatedAt int64    `json:"created_at"`
	Entries   int      `json:"entries"`
	Bytes     int64    `json:"bytes"`
	Skipped   []string `json:"skipped,omitempty"`
	Note      string   `json:"note,omitempty"`
}

// BackupInfo 一次备份的产物信息。
type BackupInfo struct {
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	Entries   int       `json:"entries"`
	Bytes     int64     `json:"bytes"`
	Skipped   []string  `json:"skipped,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// RestoreResult 恢复结果（本实现只校验 + 暂存，不覆盖生产数据）。
type RestoreResult struct {
	StagingPath     string   `json:"staging_path"`
	Entries         int      `json:"entries"`
	Bytes           int64    `json:"bytes"`
	Skipped         []string `json:"skipped,omitempty"`
	RequiresRestart bool     `json:"requires_restart"`
	Note            string   `json:"note"`
}

// TempZipPath 申请一个临时 zip 路径（落 data/tmp，与数据同盘）。
func (s *BackupService) TempZipPath(prefix string) (string, error) {
	if err := os.MkdirAll(s.tmpDir, 0o755); err != nil {
		return "", fmt.Errorf("创建临时目录失败: %w", err)
	}
	f, err := os.CreateTemp(s.tmpDir, prefix+"-*.part")
	if err != nil {
		return "", fmt.Errorf("创建临时文件失败: %w", err)
	}
	p := f.Name()
	if cErr := f.Close(); cErr != nil {
		_ = os.Remove(p)
		return "", fmt.Errorf("关闭临时文件失败: %w", cErr)
	}
	return p, nil
}

// RemoveArtifact 清理临时产物（不存在不算错误）。
func (s *BackupService) RemoveArtifact(path string) {
	if strings.TrimSpace(path) != "" {
		_ = os.Remove(path)
	}
}

// CreateBackup 把 dataDir 全量归档到 dstPath（*_test.go / handler 共用）。
//
// 跳过项见包注释：-wal / -shm / tmp/** / *.part / restore-staging-*。
func (s *BackupService) CreateBackup(ctx context.Context, dstPath string) (BackupInfo, error) {
	var info BackupInfo
	if s == nil || strings.TrimSpace(s.dataDir) == "" {
		return info, apperr.New(apperr.CodeUnavailable, "备份子系统未接线（数据目录未知）")
	}
	if strings.TrimSpace(dstPath) == "" {
		return info, invalidArg("备份目标路径不能为空")
	}
	f, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return info, apperr.Wrap(err, apperr.CodeInternal, "创建备份文件失败")
	}
	zw := zip.NewWriter(f)
	skipped := []string{}

	walkErr := filepath.WalkDir(s.dataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if eErr := ctx.Err(); eErr != nil {
			return eErr
		}
		rel, relErr := filepath.Rel(s.dataDir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if backupSkipDir(rel) {
				skipped = append(skipped, rel+"/")
				return filepath.SkipDir
			}
			return nil
		}
		// 备份产物自身不得进包（dstPath 可能正落在 data/ 内）
		if samePath(path, dstPath) {
			return nil
		}
		if backupSkipFile(rel) {
			skipped = append(skipped, rel)
			return nil
		}
		fi, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		if !fi.Mode().IsRegular() {
			skipped = append(skipped, rel)
			return nil
		}
		w, cErr := zw.Create(rel)
		if cErr != nil {
			return cErr
		}
		src, oErr := os.Open(path)
		if oErr != nil {
			return oErr
		}
		// 逐文件即时关闭：WalkDir 可能遍历上万文件，defer 会耗尽句柄。
		n, cpErr := io.Copy(w, src)
		_ = src.Close()
		if cpErr != nil {
			return cpErr
		}
		info.Entries++
		info.Bytes += n
		return nil
	})
	var manifestErr error
	if walkErr == nil {
		manifestErr = writeZipJSON(zw, backupManifestName, backupManifest{
			Schema:    backupSchema,
			CreatedAt: s.now().UnixMilli(),
			Entries:   info.Entries,
			Bytes:     info.Bytes,
			Skipped:   skipped,
			Note:      "文件级快照（跳过 -wal/-shm/tmp/*.part）；非 SQLite 事务一致性快照",
		})
	}
	closeErr := zw.Close()
	fileErr := f.Close()
	if walkErr != nil {
		_ = os.Remove(dstPath)
		if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
			return BackupInfo{}, apperr.Wrap(walkErr, apperr.CodeInternal, "备份被取消")
		}
		return BackupInfo{}, apperr.Wrap(walkErr, apperr.CodeInternal, "归档数据目录失败")
	}
	if manifestErr != nil {
		_ = os.Remove(dstPath)
		return BackupInfo{}, apperr.Wrap(manifestErr, apperr.CodeInternal, "写备份清单失败")
	}
	if closeErr != nil {
		_ = os.Remove(dstPath)
		return BackupInfo{}, apperr.Wrap(closeErr, apperr.CodeInternal, "关闭备份流失败")
	}
	if fileErr != nil {
		_ = os.Remove(dstPath)
		return BackupInfo{}, apperr.Wrap(fileErr, apperr.CodeInternal, "关闭备份文件失败")
	}
	fi, statErr := os.Stat(dstPath)
	if statErr != nil {
		return BackupInfo{}, apperr.Wrap(statErr, apperr.CodeInternal, "读取备份文件信息失败")
	}
	info.Path = dstPath
	info.Size = fi.Size()
	info.Skipped = skipped
	info.CreatedAt = s.now()
	return info, nil
}

// Restore 校验并解压备份包到独立暂存目录（**不覆盖**生产数据目录）。
func (s *BackupService) Restore(ctx context.Context, zipPath string) (RestoreResult, error) {
	var res RestoreResult
	if s == nil || strings.TrimSpace(s.dataDir) == "" {
		return res, apperr.New(apperr.CodeUnavailable, "备份子系统未接线（数据目录未知）")
	}
	abs, absErr := filepath.Abs(strings.TrimSpace(zipPath))
	if absErr != nil {
		return res, invalidArg("解析备份包路径失败: %v", absErr)
	}
	zr, err := zip.OpenReader(abs)
	if err != nil {
		return res, invalidArg("备份包不是合法 zip: %v", err)
	}
	defer func() { _ = zr.Close() }()

	if len(zr.File) > maxBackupEntries {
		return res, invalidArg("备份包条目数 %d 超过上限 %d", len(zr.File), maxBackupEntries)
	}

	// ① 先整体校验（非法即 400，且此时尚未创建任何暂存内容）
	var total int64
	seen := make(map[string]struct{}, len(zr.File))
	safeNames := make([]string, len(zr.File))
	for i, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return res, apperr.Wrap(err, apperr.CodeInternal, "恢复被取消")
		}
		if f.Mode()&os.ModeSymlink != 0 {
			return res, invalidArg("备份包含符号链接条目（拒绝）: %s", f.Name)
		}
		name, nErr := sanitizeZipEntry(f.Name)
		if nErr != nil {
			return res, nErr
		}
		if f.FileInfo().IsDir() {
			safeNames[i] = name + "/"
			continue
		}
		if f.UncompressedSize64 > uint64(maxRestoreEntryBytes) {
			return res, invalidArg("备份包条目 %s 解压体积超上限", f.Name)
		}
		total += int64(f.UncompressedSize64)
		if total > maxRestoreBytes {
			return res, invalidArg("备份包解压总体积超过上限 %d 字节", maxRestoreBytes)
		}
		if _, dup := seen[name]; dup {
			return res, invalidArg("备份包条目路径重复: %s", name)
		}
		seen[name] = struct{}{}
		safeNames[i] = name
	}
	if _, ok := seen[backupManifestName]; !ok {
		return res, invalidArg("备份包缺少 %s（非本服务生成的备份包）", backupManifestName)
	}
	if _, ok := seen["main.db"]; !ok {
		return res, invalidArg("备份包缺少 main.db（不是全量 data 备份）")
	}

	// ② 解压到独立暂存目录（data/restore-staging-<ts>），不触碰生产数据
	staging := filepath.Join(s.dataDir, "restore-staging-"+s.now().Format("20060102-150405"))
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return res, apperr.Wrap(err, apperr.CodeInternal, "创建恢复暂存目录失败")
	}
	manifestOK := false
	for i, f := range zr.File {
		name := safeNames[i]
		dst := filepath.Join(staging, filepath.FromSlash(name))
		if !withinDir(staging, dst) {
			_ = os.RemoveAll(staging)
			return res, invalidArg("条目路径越出暂存目录: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				_ = os.RemoveAll(staging)
				return res, apperr.Wrap(err, apperr.CodeInternal, "创建暂存子目录失败")
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			_ = os.RemoveAll(staging)
			return res, apperr.Wrap(err, apperr.CodeInternal, "创建暂存子目录失败")
		}
		if err := extractZipFile(f, dst); err != nil {
			_ = os.RemoveAll(staging)
			return res, err
		}
		res.Entries++
		res.Bytes += int64(f.UncompressedSize64)
		if name == backupManifestName {
			manifestOK = true
		}
	}

	// ③ 校验包内清单版本
	if !manifestOK {
		_ = os.RemoveAll(staging)
		return res, invalidArg("备份包清单缺失（解压后未找到 %s）", backupManifestName)
	}
	var manifest backupManifest
	if err := readZipJSONFile(filepath.Join(staging, backupManifestName), &manifest); err != nil {
		_ = os.RemoveAll(staging)
		return res, invalidArg("备份包清单非法: %v", err)
	}
	if manifest.Schema <= 0 || manifest.Schema > backupSchema {
		_ = os.RemoveAll(staging)
		return res, invalidArg("备份包 schema %d 超出当前支持范围（≤%d）", manifest.Schema, backupSchema)
	}
	if _, err := os.Stat(filepath.Join(staging, "main.db")); err != nil {
		_ = os.RemoveAll(staging)
		return res, invalidArg("备份包 main.db 缺失或不可读")
	}

	res.StagingPath = staging
	res.Skipped = manifest.Skipped
	res.RequiresRestart = true
	res.Note = "已校验并解压到独立暂存目录；未覆盖运行中的 data/（SQLite 句柄与 WAL 在线替换不安全）。" +
		"启用方式：停服 → 将 staging_path 内容替换 data/ → 重启。"
	return res, nil
}

// —— 内部 ——

// backupSkipDir 备份跳过的目录（相对 dataDir 的斜杠路径）。
func backupSkipDir(rel string) bool {
	switch {
	case rel == "tmp":
		return true
	case strings.HasPrefix(rel, "restore-staging-"):
		return true
	default:
		return false
	}
}

// backupSkipFile 备份跳过的文件。
//
// 说明：**不**按 .zip 后缀跳过——会话导出包（data/sessions/<sid>/bundle/*.zip）属于
// 用户数据，全量备份必须包含；备份产物自身由 tmp 目录与 samePath 排除。
func backupSkipFile(rel string) bool {
	switch {
	case strings.HasSuffix(rel, "-wal"), strings.HasSuffix(rel, "-shm"):
		return true
	case strings.HasSuffix(rel, ".part"):
		return true
	default:
		return false
	}
}

func samePath(a, b string) bool {
	aa, err := filepath.Abs(a)
	if err != nil {
		return false
	}
	bb, err := filepath.Abs(b)
	if err != nil {
		return false
	}
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}

// sanitizeZipEntry 校验并归一化 zip 条目名（拒绝穿越/绝对/盘符/反斜杠）。
func sanitizeZipEntry(name string) (string, error) {
	raw := strings.TrimSuffix(strings.TrimSpace(name), "/")
	if raw == "" || raw == "." {
		return "", invalidArg("备份包含非法条目名: %q", name)
	}
	if strings.Contains(raw, `\`) || strings.Contains(raw, ":") {
		return "", invalidArg("备份包含非法条目名（反斜杠/盘符）: %q", name)
	}
	if strings.HasPrefix(raw, "/") {
		return "", invalidArg("备份包含绝对路径条目: %q", name)
	}
	clean := path_Clean(raw)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", invalidArg("备份包含路径穿越条目: %q", name)
	}
	return clean, nil
}

// path_Clean 与 filepath.Clean 同语义但固定按斜杠处理（zip 规范）。
func path_Clean(p string) string {
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, seg := range parts {
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			} else {
				return ".."
			}
		default:
			out = append(out, seg)
		}
	}
	if len(out) == 0 {
		return "."
	}
	return strings.Join(out, "/")
}

// extractZipFile 解压单个条目（限流写入，防止声明体积与实际不符）。
func extractZipFile(f *zip.File, dst string) error {
	rc, err := f.Open()
	if err != nil {
		return invalidArg("读取备份包条目失败: %v", err)
	}
	defer func() { _ = rc.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return apperr.Wrap(err, apperr.CodeInternal, "创建暂存文件失败")
	}
	limited := io.LimitReader(rc, maxRestoreEntryBytes+1)
	n, cpErr := io.Copy(out, limited)
	closeErr := out.Close()
	if cpErr != nil {
		_ = os.Remove(dst)
		return apperr.Wrap(cpErr, apperr.CodeInternal, "解压条目失败")
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return apperr.Wrap(closeErr, apperr.CodeInternal, "写入暂存文件失败")
	}
	if n > maxRestoreEntryBytes {
		_ = os.Remove(dst)
		return invalidArg("备份包条目 %s 实际解压体积超上限", f.Name)
	}
	return nil
}

// readZipJSONFile 读取 JSON 文件（清单）。
func readZipJSONFile(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return fmt.Errorf("清单文件过大（%s 字节）", strconv.Itoa(len(data)))
	}
	return json.Unmarshal(data, dst)
}
