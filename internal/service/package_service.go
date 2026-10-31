package service

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"harness-go/internal/apperr"
	"harness-go/internal/market"
	"harness-go/internal/mcp"
	"harness-go/internal/store"
	"harness-go/internal/tools/registry"
	"harness-go/pkg/toolapi"
)

// PackageService 本地工具包用例（阶段 E / pkg.zip 安装・卸载・回滚）。
//
// 安装链路（docs/插件体系设计.md §3.5 替代落地方案）：
//
//	zip 解析 → 完整性（DIGESTS）→ 签名（ed25519 受信公钥）→ 信任策略
//	→ 解压 data/packages/<id>/<ver>/ → 子进程 stdio → MCP 握手
//	→ 注册统一 Registry（namespace pkg:<id>）→ 落库 tool_packages
//
// 出口标准：装未签名包被拒；工具包子进程隔离生效；卸载后索引一致。
type PackageService struct {
	main          *store.MainStore
	mgr           *mcp.Manager
	reg           *registry.Registry
	dataDir       string
	trustedKeys   []ed25519.PublicKey
	allowUnsigned bool
	logger        *slog.Logger
}

// PackageServiceOptions 依赖。
type PackageServiceOptions struct {
	Main          *store.MainStore
	Manager       *mcp.Manager
	Registry      *registry.Registry
	DataDir       string
	TrustedKeys   []ed25519.PublicKey
	AllowUnsigned bool
	Logger        *slog.Logger
}

// NewPackageService 创建工具包用例。
func NewPackageService(opts PackageServiceOptions) *PackageService {
	return &PackageService{
		main:          opts.Main,
		mgr:           opts.Manager,
		reg:           opts.Registry,
		dataDir:       opts.DataDir,
		trustedKeys:   opts.TrustedKeys,
		allowUnsigned: opts.AllowUnsigned,
		logger:        opts.Logger,
	}
}

// PackagesDir 返回包安装根目录（data/packages）。
func (s *PackageService) PackagesDir() string {
	return filepath.Join(s.dataDir, "packages")
}

// View 工具包 API 视图。
type PackageView struct {
	ID          string   `json:"id"`
	Version     string   `json:"version"`
	PreviousVer string   `json:"previous_version,omitempty"`
	Name        string   `json:"name"`
	Trust       string   `json:"trust"`
	Namespace   string   `json:"namespace"`
	Enabled     bool     `json:"enabled"`
	Description string   `json:"description,omitempty"`
	ToolNames   []string `json:"tool_names,omitempty"`
	State       string   `json:"state,omitempty"`
	LastError   string   `json:"last_error,omitempty"`
}

// InstallResult 安装结果。
type InstallResult struct {
	View   PackageView `json:"view"`
	Signed bool        `json:"signed"`
	Tools  int         `json:"tools"`
}

// Install 安装本地工具包 zip。
//
// 信任策略（硬约束，docs/MCP接入设计.md §10）：
//   - 签名存在但校验失败 → 一律硬拒（防篡改，不受 allow_unsigned 影响）；
//   - 未签名 → 默认拒绝（ErrUnsigned），仅显式 allow_unsigned 放行；
//   - 受信公钥列表为空 → 签名包无法证明来源，同样硬拒。
func (s *PackageService) Install(ctx context.Context, zipPath string) (*InstallResult, error) {
	pkg, err := market.Open(zipPath, s.trustedKeys)
	if err != nil {
		// market 已区分「未签名」（策略可放行）与「签名非法/内容篡改」（一律硬拒）；
		// 归一为 4xx 应用错误（422 Unprocessable Entity），绝不落 500。
		return nil, marketErrToAppErr(err)
	}
	if pkg.Trust == market.TrustUnsigned && !s.allowUnsigned {
		s.log().Warn("拒绝安装未签名工具包", "id", pkg.Manifest.ID, "version", pkg.Manifest.Version)
		return nil, marketErrToAppErr(fmt.Errorf("%w: id=%s version=%s", market.ErrUnsigned, pkg.Manifest.ID, pkg.Manifest.Version))
	}
	rel, _, berr := pkg.Binary()
	if berr != nil {
		return nil, berr
	}

	// 升级路径：同 ID 已安装 → 旧版本目录保留（回滚指针），旧连接先停
	old, _ := s.main.Packages().Get(ctx, pkg.Manifest.ID)
	if old != nil && old.Version == pkg.Manifest.Version && old.Enabled {
		return nil, apperr.New(apperr.CodeConflict, "工具包 %s@%s 已安装", pkg.Manifest.ID, pkg.Manifest.Version)
	}

	root, err := pkg.Extract(s.PackagesDir())
	if err != nil {
		return nil, err
	}
	// Windows 下 .exe 自带可执行位；unix 由 0o755 写入保证。
	_ = os.Chmod(filepath.Join(root, filepath.FromSlash(rel)), 0o755)

	// 子进程 stdio → MCP 握手（与 MCP Server 同一套连接代码路径）
	binAbs := filepath.Join(root, filepath.FromSlash(rel))
	cfg := mcp.ServerConfig{
		ID:         pkg.Manifest.ID,
		Name:       pkg.Manifest.Name,
		Transport:  mcp.TransportStdio,
		Command:    binAbs,
		Cwd:        root,
		Enabled:    true,
		PluginKind: toolapi.KindPackage,
	}
	if args, ok := pkg.Manifest.Meta["args"].([]any); ok {
		for _, a := range args {
			if str, ok := a.(string); ok {
				cfg.Args = append(cfg.Args, str)
			}
		}
	}
	// 工具集以 manifest 声明为准（包发布者的承诺面）：二进制多暴露的工具一律过滤，
	// 防「声明最小权限、运行时多注册」的越权面。
	for _, t := range pkg.Manifest.Tools {
		cfg.AllowTools = append(cfg.AllowTools, t.Name)
	}
	// 升级：先停旧连接再起新连接
	if old != nil {
		_ = s.reg.UnregisterPlugin(s.PluginIDOf(old.ID))
		_ = s.mgr.Stop(old.ID)
	}
	if err := s.mgr.Start(ctx, cfg); err != nil {
		// 连接失败 = 包不可用：清理本次解压（半安装不留残骸），保留旧版本
		_ = os.RemoveAll(root)
		if old != nil && old.Version != pkg.Manifest.Version {
			_ = s.restoreConnection(ctx, old) // 尽力恢复旧版本
		}
		return nil, fmt.Errorf("market: 包 %s@%s 握手失败（已回滚安装目录）: %w",
			pkg.Manifest.ID, pkg.Manifest.Version, err)
	}

	// 注册 + 落库
	h, ok := s.mgr.Handle(pkg.Manifest.ID)
	if !ok {
		return nil, apperr.New(apperr.CodeInternal, "工具包连接句柄丢失")
	}
	if err := s.reg.RegisterPlugin(ctx, h.Manifest(), h); err != nil {
		_ = s.mgr.Stop(pkg.Manifest.ID)
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("market: 注册工具失败: %w", err)
	}
	prev := ""
	if old != nil && old.Version != pkg.Manifest.Version {
		prev = old.Version // 旧版本目录保留 → 可回滚
	}
	row := &store.PackageRow{
		ID:              pkg.Manifest.ID,
		Version:         pkg.Manifest.Version,
		PreviousVersion: prev,
		Name:            pkg.Manifest.Name,
		Trust:           string(pkg.Trust),
		Namespace:       "pkg:" + pkg.Manifest.ID,
		InstallPath:     filepath.ToSlash(filepath.Join("packages", pkg.Manifest.ID, pkg.Manifest.Version)),
		BinaryRelPath:   rel,
		ManifestJSON:    string(manifestJSON(pkg.Manifest)),
		Signature:       pkg.SignatureHex(),
		Enabled:         true,
	}
	if err := s.main.Packages().Upsert(ctx, row); err != nil {
		_ = s.reg.UnregisterPlugin(s.PluginIDOf(pkg.Manifest.ID))
		_ = s.mgr.Stop(pkg.Manifest.ID)
		return nil, err
	}
	s.log().Info("工具包安装成功",
		"id", row.ID, "version", row.Version, "trust", row.Trust,
		"tools", len(h.Tools()), "platform", runtime.GOOS+"/"+runtime.GOARCH)
	return &InstallResult{
		View:   s.view(*row, h),
		Signed: pkg.Trust == market.TrustSigned,
		Tools:  len(h.Tools()),
	}, nil
}

// Uninstall 卸载：注销工具 → 关连接 → 删行 → 清目录（出口标准「卸载后索引一致」）。
func (s *PackageService) Uninstall(ctx context.Context, id string) error {
	row, err := s.main.Packages().Get(ctx, id)
	if err != nil {
		return err
	}
	if row == nil {
		return apperr.New(apperr.CodeNotFound, "工具包不存在")
	}
	_ = s.reg.UnregisterPlugin(s.PluginIDOf(id))
	_ = s.mgr.Stop(id)
	if err := s.main.Packages().Delete(ctx, id); err != nil {
		return err
	}
	// 删除该包全部版本目录（含回滚副本）
	if err := os.RemoveAll(filepath.Join(s.PackagesDir(), id)); err != nil {
		return fmt.Errorf("market: 清理安装目录失败: %w", err)
	}
	s.log().Info("工具包已卸载", "id", id, "version", row.Version)
	return nil
}

// Rollback 回滚到 previous_version（旧版本目录在升级时保留）。
func (s *PackageService) Rollback(ctx context.Context, id string) (*PackageView, error) {
	row, err := s.main.Packages().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, apperr.New(apperr.CodeNotFound, "工具包不存在")
	}
	if row.PreviousVersion == "" {
		return nil, apperr.New(apperr.CodeInvalidArgument, "工具包 %s 无可回滚版本", id)
	}
	prevPath := filepath.Join(s.dataDir, filepath.FromSlash(row.InstallPath))
	// InstallPath 指向当前版本；旧版本目录 = packages/<id>/<previous_version>
	prevPath = filepath.Join(s.PackagesDir(), id, row.PreviousVersion)
	if _, err := os.Stat(prevPath); err != nil {
		return nil, apperr.New(apperr.CodeNotFound, "旧版本目录不存在（无法回滚）: %s", row.PreviousVersion)
	}
	// 从 manifest_json 还原旧版本元数据（版本号、二进制名按目录重解析）
	var m toolapi.Manifest
	if err := jsonUnmarshal([]byte(row.ManifestJSON), &m); err != nil {
		return nil, apperr.New(apperr.CodeInternal, "工具包 manifest 损坏: %v", err)
	}
	m.Version = row.PreviousVersion

	_ = s.reg.UnregisterPlugin(s.PluginIDOf(id))
	_ = s.mgr.Stop(id)

	binRel, err := findBinaryIn(prevPath)
	if err != nil {
		return nil, apperr.New(apperr.CodeInternal, "旧版本二进制缺失: %v", err)
	}
	cfg := mcp.ServerConfig{
		ID: id, Name: m.Name, Transport: mcp.TransportStdio,
		Command: filepath.Join(prevPath, filepath.FromSlash(binRel)),
		Cwd:     prevPath, Enabled: true,
		PluginKind: toolapi.KindPackage,
	}
	if err := s.mgr.Start(ctx, cfg); err != nil {
		return nil, fmt.Errorf("market: 回滚后握手失败: %w", err)
	}
	h, ok := s.mgr.Handle(id)
	if !ok {
		return nil, apperr.New(apperr.CodeInternal, "回滚连接句柄丢失")
	}
	if err := s.reg.RegisterPlugin(ctx, h.Manifest(), h); err != nil {
		return nil, err
	}
	row.Version, row.PreviousVersion = row.PreviousVersion, row.Version
	row.InstallPath = filepath.ToSlash(filepath.Join("packages", id, row.Version))
	row.BinaryRelPath = binRel
	if err := s.main.Packages().Upsert(ctx, row); err != nil {
		return nil, err
	}
	s.log().Info("工具包已回滚", "id", id, "version", row.Version)
	v := s.view(*row, h)
	return &v, nil
}

// SetEnabled 启停（禁用 = 注销工具，保留安装与配置）。
func (s *PackageService) SetEnabled(ctx context.Context, id string, enabled bool) error {
	row, err := s.main.Packages().Get(ctx, id)
	if err != nil {
		return err
	}
	if row == nil {
		return apperr.New(apperr.CodeNotFound, "工具包不存在")
	}
	if err := s.main.Packages().SetEnabled(ctx, id, enabled); err != nil {
		return err
	}
	if !enabled {
		_ = s.reg.UnregisterPlugin(s.PluginIDOf(id))
		_ = s.mgr.Stop(id)
		return nil
	}
	return s.restoreConnection(ctx, row)
}

// List 全部工具包视图。
func (s *PackageService) List(ctx context.Context) ([]PackageView, error) {
	rows, err := s.main.Packages().List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]PackageView, 0, len(rows))
	for _, row := range rows {
		h, running := s.mgr.Handle(row.ID)
		if !running {
			h = nil
		}
		out = append(out, s.view(row, h))
	}
	return out, nil
}

// RestoreAll 启动期恢复：对 enabled 包重连注册（目录缺失/握手失败 → 隔离记录）。
func (s *PackageService) RestoreAll(ctx context.Context) {
	rows, err := s.main.Packages().List(ctx)
	if err != nil {
		s.log().Warn("读取工具包列表失败（跳过恢复）", "err", err)
		return
	}
	n := 0
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		if err := s.restoreConnection(ctx, &row); err != nil {
			s.log().Warn("工具包恢复失败（已隔离）", "id", row.ID, "err", err)
			continue
		}
		n++
	}
	if n > 0 {
		s.log().Info("已恢复本地工具包", "count", n)
	}
}

// restoreConnection 从安装目录重连并注册。
func (s *PackageService) restoreConnection(ctx context.Context, row *store.PackageRow) error {
	binAbs := filepath.Join(s.dataDir, filepath.FromSlash(row.InstallPath), filepath.FromSlash(row.BinaryRelPath))
	if _, err := os.Stat(binAbs); err != nil {
		return fmt.Errorf("market: 二进制缺失: %w", err)
	}
	cfg := mcp.ServerConfig{
		ID: row.ID, Name: row.Name, Transport: mcp.TransportStdio,
		Command: binAbs, Cwd: filepath.Dir(binAbs), Enabled: true,
		PluginKind: toolapi.KindPackage,
	}
	_ = s.mgr.Stop(row.ID) // 幂等：重连前先关旧连接
	if err := s.mgr.Start(ctx, cfg); err != nil {
		return err
	}
	h, ok := s.mgr.Handle(row.ID)
	if !ok {
		return fmt.Errorf("market: 连接句柄丢失")
	}
	if err := s.reg.RegisterPlugin(ctx, h.Manifest(), h); err != nil {
		return err
	}
	return nil
}

// view 行 → API 视图（运行态合并）。
func (s *PackageService) view(row store.PackageRow, h *mcp.Handle) PackageView {
	v := PackageView{
		ID: row.ID, Version: row.Version, PreviousVer: row.PreviousVersion,
		Name: row.Name, Trust: row.Trust, Namespace: row.Namespace, Enabled: row.Enabled,
	}
	var m toolapi.Manifest
	if jsonUnmarshal([]byte(row.ManifestJSON), &m) == nil {
		v.Description = m.Description
		for _, t := range m.Tools {
			v.ToolNames = append(v.ToolNames, t.Name)
		}
	}
	if h != nil {
		if st := h.Health(); st.Status != toolapi.HealthAvailable {
			v.State = string(st.Status)
			v.LastError = st.Message
		} else {
			v.State = string(mcp.StateReady)
		}
	}
	return v
}

// PluginIDOf 插件 ID 规则（pkg:<id>）。
func (s *PackageService) PluginIDOf(id string) string { return "pkg:" + id }

func (s *PackageService) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// marketErrToAppErr 把包格式/签名/信任错误映射为 422 应用错误（机器可判 + 可读文案）。
func marketErrToAppErr(err error) error {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return err // 已是应用错误
	}
	if errors.Is(err, market.ErrUnsigned) || errors.Is(err, market.ErrBadSignature) ||
		errors.Is(err, market.ErrBadDigest) || errors.Is(err, market.ErrNoBinary) {
		return apperr.New(apperr.CodeInvalidArgument, "%s", err.Error())
	}
	return err
}

// manifestJSON 序列化清单（落库真源）。
func manifestJSON(m toolapi.Manifest) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// jsonUnmarshal 宽松 JSON 解析（清单真源由安装时校验，此处只做还原）。
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// findBinaryIn 在已解压的包目录中定位当前平台二进制（相对路径）。
func findBinaryIn(root string) (string, error) {
	dir := filepath.Join(root, market.BinaryDir, runtime.GOOS+"_"+runtime.GOARCH)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var found []string
	for _, e := range entries {
		if !e.IsDir() {
			found = append(found, filepath.ToSlash(filepath.Join(market.BinaryDir, runtime.GOOS+"_"+runtime.GOARCH, e.Name())))
		}
	}
	if len(found) == 1 {
		return found[0], nil
	}
	if len(found) == 0 {
		return "", market.ErrNoBinary
	}
	return "", fmt.Errorf("%w: 平台目录下有 %d 个文件", market.ErrNoBinary, len(found))
}
