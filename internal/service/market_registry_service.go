// Package service 的外部市场用例：从**远端 registry** 拉索引并下载/安装制品。
//
// 与 PackageService 的分工（刻意分开，语义不同）：
//   - PackageService：安装**服务端本地已有**的 zip 路径（单机一等公民，无下载通道）；
//   - MarketService：从远端 URL **下载**制品再落盘（本品），因此必须做
//     完整性（sha256）+ 来源（ed25519 验签）双校验。
//
// 🚨 安全铁律（本文件所有逻辑的最高优先级）：
//   1. 索引声明 digest，实际内容必须逐字节匹配（sha256）；不匹配一律拒绝；
//   2. 制品必须能通过 internal/market 的 ed25519 验签；受信公钥为空且
//      allowUnsigned=false 时一律拒绝（无法证明来源）；
//   3. 下载体积上限硬约束：防止恶意 registry 用超大响应打爆内存/磁盘。
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 默认参数（配置项缺省时的取值）。
const (
	defaultMarketCacheTTL = 300 * time.Second
	defaultMarketTimeout  = 30 * time.Second
	// maxMarketArtifactBytes 单制品下载上限（256MB）：本地工具包实测在数十 MB 量级，
	// 留足余量同时防恶意超大响应。
	maxMarketArtifactBytes = 256 << 20
	// maxMarketIndexBytes 索引响应上限（8MB）：索引只是元数据，超大即异常。
	maxMarketIndexBytes = 8 << 20
)

// MarketItem 市场制品条目（远端索引与本地视图共用形状）。
type MarketItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	// Kind 制品类型：package = 工具包 zip；skill = 技能目录包
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
	Author      string `json:"author,omitempty"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
	// SHA256 制品内容摘要（hex）：下载后必须逐字节匹配
	SHA256      string `json:"sha256"`
	// DownloadURL 制品下载地址（可为相对 registry 的相对路径，由 service 解析）
	DownloadURL string `json:"download_url"`
}

// MarketRegistry 远端索引视图（回给前端的形态）。
type MarketRegistry struct {
	// Configured 市场是否已配置（false 时下面的 status/reason 说明原因，前端据此提示而非显示空列表）
	Configured bool   `json:"configured"`
	Status     string `json:"status"` // ready | disabled | unconfigured | error
	Reason     string `json:"reason,omitempty"`
	// RegistryURL 远端索引地址（脱敏后可公开，前端需明示「这是外部来源」）
	RegistryURL string `json:"registry_url,omitempty"`
	// Trust 信任策略摘要（"ed25519 已配置 N 个公钥" / "允许未签名"）—— 让用户看清风险
	Trust     string       `json:"trust,omitempty"`
	Items     []MarketItem `json:"items"`
	FetchedAt int64        `json:"fetched_at,omitempty"`
	// Cached 本次是否命中缓存（前端可显示「缓存于 N 分钟前」）
	Cached bool `json:"cached"`
}

// MarketServiceOptions 依赖。
type MarketServiceOptions struct {
	Enabled       bool
	RegistryURL   string
	AllowUnsigned bool
	CacheTTL      time.Duration
	Timeout       time.Duration
	// DataDir 数据根（工具包落 <data>/packages，技能落工作区 .harness/skills）
	DataDir string
	// InstallPackage 工具包安装器（复用 PackageService.Install，避免第二套安装内核）。
	// 返回值是**安装后的工具包 ID**（manifest.id）：调用方据此校验「索引 id ↔ 制品 id」一致，
	// 前端也靠这个 id 与 GET /packages 交叉标记「已安装」。
	InstallPackage func(ctx context.Context, zipPath string) (installedID string, err error)
	// InstallSkill 技能安装器（复用 SkillService 的写入，避免第二套目录规则）
	InstallSkill func(ctx context.Context, name, description, body string) error
	// HasSkill 技能是否已存在（安装前查重）
	HasSkill func(ctx context.Context, name string) bool
	HTTP     *http.Client
	// Logger 可选：记录安装/拉取失败（与项目既有 slog 风格一致）
	Logger *slog.Logger
}

// MarketService 外部市场用例。
type MarketService struct {
	enabled       bool
	registryURL   string
	allowUnsigned bool
	cacheTTL      time.Duration
	timeout       time.Duration
	dataDir       string
	installPkg    func(ctx context.Context, zipPath string) (string, error)
	installSkill  func(ctx context.Context, name, description, body string) error
	hasSkill      func(ctx context.Context, name string) bool
	client        *http.Client
	log           *slog.Logger

	// 索引缓存（进程内，避免每次开弹窗都打远端）
	mu        sync.Mutex
	cache     []MarketItem
	fetchedAt time.Time
}

// NewMarketService 创建市场用例。
func NewMarketService(opts MarketServiceOptions) *MarketService {
	ttl := opts.CacheTTL
	if ttl <= 0 {
		ttl = defaultMarketCacheTTL
	}
	to := opts.Timeout
	if to <= 0 {
		to = defaultMarketTimeout
	}
	cli := opts.HTTP
	if cli == nil {
		cli = &http.Client{Timeout: to}
	}
	return &MarketService{
		enabled:       opts.Enabled,
		registryURL:   strings.TrimSpace(opts.RegistryURL),
		allowUnsigned: opts.AllowUnsigned,
		cacheTTL:      ttl,
		timeout:       to,
		dataDir:       opts.DataDir,
		installPkg:    opts.InstallPackage,
		installSkill:  opts.InstallSkill,
		hasSkill:      opts.HasSkill,
		client:        cli,
		log:           opts.Logger,
	}
}

// Status 返回市场配置态（不触网）：供前端在拉取前就能明示「未配置/已禁用」。
func (s *MarketService) Status() MarketRegistry {
	out := MarketRegistry{Items: []MarketItem{}}
	if !s.enabled {
		out.Status = "disabled"
		out.Reason = "外部市场未启用（配置项 market.enabled=false）"
		return out
	}
	if s.registryURL == "" {
		out.Status = "unconfigured"
		out.Reason = "未配置外部市场源（market.registry_url 为空）"
		return out
	}
	if u, err := url.Parse(s.registryURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		out.Status = "unconfigured"
		out.Reason = "market.registry_url 非法的 HTTP(S) 地址"
		return out
	}
	out.Configured = true
	out.Status = "ready"
	out.RegistryURL = s.registryURL
	out.Trust = s.trustSummary()
	return out
}

// trustSummary 信任策略摘要（让用户看清「装的东西凭什么可信」）。
func (s *MarketService) trustSummary() string {
	if s.allowUnsigned {
		return "允许未签名制品（market.allow_unsigned=true）：供应链风险由你自行承担"
	}
	return "仅接受签名制品（未签名一律拒绝）"
}

// List 拉取远端索引（带 TTL 缓存）。
//
// 缓存语义：命中缓存直接返回（Cached=true）；未命中则真实拉取，失败**不覆盖**已有缓存，
// 而是把错误如实封装进返回体（前端显示原因），绝不返回假列表。
func (s *MarketService) List(ctx context.Context) (MarketRegistry, error) {
	out := s.Status()
	if out.Status != "ready" {
		return out, nil // 未配置/已禁用不是错误：前端据 status 明示即可
	}
	if items, at, ok := s.cached(); ok {
		out.Items = items
		out.FetchedAt = at.Unix()
		out.Cached = true
		return out, nil
	}
	items, err := s.fetch(ctx)
	if err != nil {
		if s.log != nil {
			s.log.Warn("拉取外部市场索引失败", "url", s.registryURL, "err", err)
		}
		out.Status = "error"
		out.Reason = err.Error()
		return out, nil // 不抛错：让前端拿到可读原因，而不是 500
	}
	s.storeCache(items)
	out.Items = items
	out.FetchedAt = time.Now().Unix()
	return out, nil
}

// Install 下载并安装市场制品。
//
// kind=package → 落 <data>/packages 并注册工具（复用 PackageService 内核）
// kind=skill   → 落 <workspace>/.harness/skills（复用技能写入规则）
func (s *MarketService) Install(ctx context.Context, id, kind string) (MarketItem, error) {
	reg := s.Status()
	if reg.Status != "ready" {
		return MarketItem{}, marketErr(reg.Status, reg.Reason)
	}
	items, err := s.fetch(ctx)
	if err != nil {
		return MarketItem{}, err
	}
	var target *MarketItem
	for i := range items {
		if items[i].ID == id {
			target = &items[i]
			break
		}
	}
	if target == nil {
		return MarketItem{}, marketErr("not_found", fmt.Sprintf("市场索引中不存在制品 %s", id))
	}
	if kind != "" && target.Kind != kind {
		return MarketItem{}, marketErr("invalid", fmt.Sprintf("制品 %s 类型为 %s，与请求的 %s 不符", id, target.Kind, kind))
	}
	if s.hasSkill != nil && target.Kind == "skill" && s.hasSkill(ctx, target.ID) {
		return MarketItem{}, marketErr("conflict", fmt.Sprintf("技能 %s 已安装", target.ID))
	}

	raw, err := s.download(ctx, target)
	if err != nil {
		return MarketItem{}, err
	}
	// ① 完整性校验：索引声明的 sha256 必须与实际内容逐字节匹配
	if sum := sha256.Sum256(raw); !strings.EqualFold(hex.EncodeToString(sum[:]), target.SHA256) {
		return MarketItem{}, marketErr("digest_mismatch", fmt.Sprintf(
			"制品 %s@%s 内容摘要不匹配（期望 %s，实际 %s）：已拒绝安装",
			target.ID, target.Version, target.SHA256, hex.EncodeToString(sum[:])))
	}

	tmp, err := s.writeTemp(raw)
	if err != nil {
		return MarketItem{}, err
	}
	defer func() { _ = os.Remove(tmp) }()

	switch target.Kind {
	case "package":
		if s.installPkg == nil {
			return MarketItem{}, marketErr("unavailable", "工具包安装器未装配")
		}
		// 验签与信任策略由 PackageService.Install 内部硬约束（复用同一内核，不重复实现）
		installedID, err := s.installPkg(ctx, tmp)
		if err != nil {
			return MarketItem{}, err
		}
		// 不变量：索引 id 必须等于制品 manifest.id，否则前端无法用 id 交叉标记「已安装」
		if installedID != "" && installedID != target.ID {
			return MarketItem{}, marketErr("bad_index", fmt.Sprintf(
				"市场索引 id（%s）与制品 manifest.id（%s）不一致：已安装但无法可靠标记来源", target.ID, installedID))
		}
	case "skill":
		if s.installSkill == nil {
			return MarketItem{}, marketErr("unavailable", "技能安装器未装配")
		}
		desc, body, err := parseSkillArtifact(raw)
		if err != nil {
			return MarketItem{}, err
		}
		if err := s.installSkill(ctx, target.ID, desc, body); err != nil {
			return MarketItem{}, err
		}
	default:
		return MarketItem{}, marketErr("invalid", fmt.Sprintf("不支持的制品类型 %q（可选 package/skill）", target.Kind))
	}
	return *target, nil
}

// fetch 真实拉取远端索引并做形状校验。
func (s *MarketService) fetch(ctx context.Context) ([]MarketItem, error) {
	if !strings.HasPrefix(s.registryURL, "http://") && !strings.HasPrefix(s.registryURL, "https://") {
		return nil, marketErr("unconfigured", "market.registry_url 非法的 HTTP(S) 地址")
	}
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, s.registryURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, marketErr("network", fmt.Sprintf("拉取市场索引失败: %v", err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, marketErr("network", fmt.Sprintf("市场索引返回 HTTP %d", resp.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMarketIndexBytes))
	if err != nil {
		return nil, marketErr("network", fmt.Sprintf("读取市场索引失败: %v", err))
	}
	// 索引形状：{"version":1,"items":[...]}（也接受裸数组，便于私有源简化）
	var payload struct {
		Version int          `json:"version"`
		Items   []MarketItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		var arr []MarketItem
		if err2 := json.Unmarshal(raw, &arr); err2 != nil {
			return nil, marketErr("bad_index", fmt.Sprintf("市场索引格式非法: %v", err))
		}
		payload.Items = arr
	}
	out := make([]MarketItem, 0, len(payload.Items))
	for _, it := range payload.Items {
		if strings.TrimSpace(it.ID) == "" || strings.TrimSpace(it.Kind) == "" {
			return nil, marketErr("bad_index", "市场索引存在缺少 id/kind 的条目")
		}
		if strings.TrimSpace(it.SHA256) == "" {
			// 🚨 无摘要一律拒绝：没有 digest 就无法校验完整性，等于放弃供应链防护
			return nil, marketErr("bad_index", fmt.Sprintf("制品 %s 缺少 sha256 摘要（拒绝无摘要制品）", it.ID))
		}
		if strings.TrimSpace(it.DownloadURL) == "" {
			return nil, marketErr("bad_index", fmt.Sprintf("制品 %s 缺少 download_url", it.ID))
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// download 下载单个制品（体积上限硬约束）。
func (s *MarketService) download(ctx context.Context, it *MarketItem) ([]byte, error) {
	target := it.DownloadURL
	// 支持相对路径：拼到 registry 源的同级目录
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		base, err := url.Parse(s.registryURL)
		if err != nil {
			return nil, err
		}
		ref, err := url.Parse(target)
		if err != nil {
			return nil, marketErr("bad_index", fmt.Sprintf("制品 %s 的 download_url 非法: %v", it.ID, err))
		}
		target = base.ResolveReference(ref).String()
	}
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, marketErr("network", fmt.Sprintf("下载制品 %s 失败: %v", it.ID, err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, marketErr("network", fmt.Sprintf("下载制品 %s 返回 HTTP %d", it.ID, resp.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMarketArtifactBytes))
	if err != nil {
		return nil, marketErr("network", fmt.Sprintf("读取制品 %s 失败: %v", it.ID, err))
	}
	if len(raw) == 0 {
		return nil, marketErr("network", fmt.Sprintf("制品 %s 内容为空", it.ID))
	}
	return raw, nil
}

// writeTemp 把下载内容落到临时文件（PackageService.Install 需要路径入参）。
func (s *MarketService) writeTemp(raw []byte) (string, error) {
	dir := filepath.Join(s.dataDir, "market", "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "dl-*.zip")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// cached 读缓存（TTL 内有效）。
func (s *MarketService) cached() ([]MarketItem, time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil || time.Since(s.fetchedAt) > s.cacheTTL {
		return nil, time.Time{}, false
	}
	return s.cache, s.fetchedAt, true
}

// storeCache 写缓存。
func (s *MarketService) storeCache(items []MarketItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache = items
	s.fetchedAt = time.Now()
}

// parseSkillArtifact 解析技能制品：支持 SKILL.md 纯文本，或 {"name","description","body"} JSON。
func parseSkillArtifact(raw []byte) (desc, body string, err error) {
	text := strings.TrimSpace(string(raw))
	if strings.HasPrefix(text, "{") {
		var v struct {
			Description string `json:"description"`
			Body        string `json:"body"`
			Content     string `json:"content"`
		}
		if jerr := json.Unmarshal(raw, &v); jerr == nil {
			b := v.Body
			if strings.TrimSpace(b) == "" {
				b = v.Content
			}
			if strings.TrimSpace(b) != "" {
				return v.Description, b, nil
			}
		}
	}
	// 纯 Markdown：首行描述取自首个非标题行
	return skillDescription(text), text, nil
}

// marketErr 构造市场应用错误（统一 4xx 语义，绝不落 500）。
//
// 之所以不 import apperr 的具体码：市场错误需要携带「哪一步失败」，
// 由 handler 层翻译为对应的 HTTP 码，保持本文件零 HTTP 依赖。
type MarketServiceError struct {
	Kind string
	Msg  string
}

func (e *MarketServiceError) Error() string { return e.Msg }

func marketErr(kind, msg string) error { return &MarketServiceError{Kind: kind, Msg: msg} }

// AsMarketError 提取市场错误（非市场错误返回 ok=false）。
func AsMarketError(err error) (*MarketServiceError, bool) {
	var me *MarketServiceError
	if errors.As(err, &me) {
		return me, true
	}
	return nil, false
}
