package service

import (
	"context"
	"log/slog"
	"sort"

	"harness-go/internal/apperr"
	"harness-go/internal/mcp"
	"harness-go/internal/secret"
	"harness-go/internal/store"
	"harness-go/internal/tools/registry"
)

// MCPService MCP Server 管理用例（阶段 E / docs/MCP接入设计.md §9）。
//
// 职责：配置 CRUD（密钥加密落库）+ 生命周期（启动/停用/重载）+ 联通测试
// + 注册表联动（外部工具与内置插件同构注册，走同一审批与注入规则）。
type MCPService struct {
	main   *store.MainStore
	mgr    *mcp.Manager
	reg    *registry.Registry
	cipher *secret.Cipher
	dep    mcp.DeploymentPolicy
	logger *slog.Logger
}

// MCPServiceOptions 依赖。
type MCPServiceOptions struct {
	Main     *store.MainStore
	Manager  *mcp.Manager
	Registry *registry.Registry
	Cipher   *secret.Cipher
	Dep      mcp.DeploymentPolicy
	Logger   *slog.Logger
}

// NewMCPService 创建 MCP 管理用例。
func NewMCPService(opts MCPServiceOptions) *MCPService {
	return &MCPService{
		main:   opts.Main,
		mgr:    opts.Manager,
		reg:    opts.Registry,
		cipher: opts.Cipher,
		dep:    opts.Dep,
		logger: opts.Logger,
	}
}

// ServerView MCP Server 的 API 视图（密钥脱敏：只回键名不回值，§10 安全边界）。
type ServerView struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Transport       string   `json:"transport"`
	Command         string   `json:"command,omitempty"`
	Args            []string `json:"args,omitempty"`
	EnvKeys         []string `json:"env_keys,omitempty"`
	Cwd             string   `json:"cwd,omitempty"`
	URL             string   `json:"url,omitempty"`
	HeaderKeys      []string `json:"header_keys,omitempty"`
	Enabled         bool     `json:"enabled"`
	TimeoutMS       int      `json:"timeout_ms"`
	MaxOutput       int      `json:"max_output"`
	DefaultRisk     string   `json:"default_risk"`
	AllowTools      []string `json:"allow_tools,omitempty"`
	DenyTools       []string `json:"deny_tools,omitempty"`
	HealthIntervalS int      `json:"health_interval_s"`
	// 运行态（未启动时为空）
	State     string `json:"state,omitempty"`
	Calls     int64  `json:"calls,omitempty"`
	Failures  int64  `json:"failures,omitempty"`
	ToolCount int    `json:"tool_count,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// CreateInput 创建/更新入参（env/headers 明文进、密文出库）。
type MCPCreateInput struct {
	Name            string            `json:"name"`
	Transport       string            `json:"transport"`
	Command         string            `json:"command,omitempty"`
	Args            []string          `json:"args,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Cwd             string            `json:"cwd,omitempty"`
	URL             string            `json:"url,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	Enabled         *bool             `json:"enabled,omitempty"`
	TimeoutMS       int               `json:"timeout_ms,omitempty"`
	MaxOutput       int               `json:"max_output,omitempty"`
	DefaultRisk     string            `json:"default_risk,omitempty"`
	AllowTools      []string          `json:"allow_tools,omitempty"`
	DenyTools       []string          `json:"deny_tools,omitempty"`
	HealthIntervalS *int              `json:"health_interval_s,omitempty"`
}

// newServerID 生成 MCP Server ID。
func newServerID() string { return newID() }

// Create 新增配置（可选立即启动）。
func (s *MCPService) Create(ctx context.Context, in MCPCreateInput, startNow bool) (*ServerView, error) {
	row, err := s.rowFromInput(newServerID(), in)
	if err != nil {
		return nil, err
	}
	if err := s.main.MCPServers().Upsert(ctx, row); err != nil {
		return nil, err
	}
	if startNow && row.Enabled {
		if serr := s.Start(ctx, row.ID); serr != nil {
			// 启动失败不回滚配置（可观测 + 可修复后 reload），返回带 failed 状态的视图
			s.log().Warn("MCP Server 启动失败（配置已保存）", "id", row.ID, "err", serr)
		}
	}
	return s.Get(ctx, row.ID)
}

// Update 全量更新配置（变更后需显式 reload 才生效——设计 §9）。
func (s *MCPService) Update(ctx context.Context, id string, in MCPCreateInput) (*ServerView, error) {
	old, err := s.main.MCPServers().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if old == nil {
		return nil, apperr.New(apperr.CodeNotFound, "MCP Server 不存在")
	}
	row, err := s.rowFromInput(id, in)
	if err != nil {
		return nil, err
	}
	row.CreatedAt = old.CreatedAt
	if err := s.main.MCPServers().Upsert(ctx, row); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// rowFromInput 入参 → 存储行（密钥加密）。
func (s *MCPService) rowFromInput(id string, in MCPCreateInput) (*store.MCPServerRow, error) {
	cfg := mcp.ServerConfig{
		ID: id, Name: in.Name,
		Transport: mcp.Transport(in.Transport),
		Command:   in.Command, Args: in.Args, Env: in.Env, Cwd: in.Cwd,
		URL: in.URL, Headers: in.Headers,
		Enabled:   in.Enabled == nil || *in.Enabled,
		TimeoutMS: in.TimeoutMS, MaxOutput: in.MaxOutput,
		DefaultRisk: in.DefaultRisk,
		AllowTools:  in.AllowTools, DenyTools: in.DenyTools,
		HealthIntervalS: defaultHealthInterval(in.HealthIntervalS),
	}
	if err := cfg.Validate(s.dep); err != nil {
		return nil, apperr.New(apperr.CodeInvalidArgument, "%s", err.Error())
	}
	cfg = cfg.Normalize() // 落库即带默认值（视图回显与运行时口径一致）
	env, err := store.NewEncryptedMap(s.cipher, in.Env)
	if err != nil {
		return nil, err
	}
	headers, err := store.NewEncryptedMap(s.cipher, in.Headers)
	if err != nil {
		return nil, err
	}
	return &store.MCPServerRow{
		ID: cfg.ID, Name: cfg.Name, Transport: string(cfg.Transport),
		Command: cfg.Command, Args: cfg.Args, Env: env, Cwd: cfg.Cwd,
		URL: cfg.URL, Headers: headers, Enabled: cfg.Enabled,
		TimeoutMS: cfg.TimeoutMS, MaxOutput: cfg.MaxOutput,
		DefaultRisk: cfg.DefaultRisk, AllowTools: cfg.AllowTools, DenyTools: cfg.DenyTools,
		HealthIntervalS: cfg.HealthIntervalS,
	}, nil
}

func defaultHealthInterval(p *int) int {
	if p == nil {
		return mcp.DefaultHealthIntervalS
	}
	return *p
}

// Get 单个视图（配置 + 运行态合并）。
func (s *MCPService) Get(ctx context.Context, id string) (*ServerView, error) {
	row, err := s.main.MCPServers().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, apperr.New(apperr.CodeNotFound, "MCP Server 不存在")
	}
	return s.view(*row), nil
}

// List 全部视图。
func (s *MCPService) List(ctx context.Context) ([]ServerView, error) {
	rows, err := s.main.MCPServers().List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ServerView, 0, len(rows))
	for _, row := range rows {
		out = append(out, *s.view(row))
	}
	return out, nil
}

// view 行 → API 视图（密钥脱敏 + 运行态合并）。
func (s *MCPService) view(row store.MCPServerRow) *ServerView {
	v := &ServerView{
		ID: row.ID, Name: row.Name, Transport: row.Transport,
		Command: row.Command, Args: row.Args, Cwd: row.Cwd, URL: row.URL,
		Enabled: row.Enabled, TimeoutMS: row.TimeoutMS, MaxOutput: row.MaxOutput,
		DefaultRisk: row.DefaultRisk, AllowTools: row.AllowTools, DenyTools: row.DenyTools,
		HealthIntervalS: row.HealthIntervalS,
	}
	if keys := row.Env.Keys(); len(keys) > 0 {
		sort.Strings(keys)
		v.EnvKeys = keys
	}
	if keys := row.Headers.Keys(); len(keys) > 0 {
		sort.Strings(keys)
		v.HeaderKeys = keys
	}
	for _, st := range s.mgr.Statuses() {
		if st.ID == row.ID {
			v.State, v.LastError = st.State, st.LastError
			v.Calls, v.Failures, v.ToolCount = st.Calls, st.Failures, st.ToolCount
			break
		}
	}
	return v
}

// toConfig 行 → 运行配置（解密密钥到内存；只在启动路径发生）。
func (s *MCPService) toConfig(row *store.MCPServerRow) (mcp.ServerConfig, error) {
	env, err := row.Env.Plain(s.cipher)
	if err != nil {
		return mcp.ServerConfig{}, apperr.New(apperr.CodeInvalidArgument, "MCP Server %s 环境变量解密失败", row.ID)
	}
	headers, err := row.Headers.Plain(s.cipher)
	if err != nil {
		return mcp.ServerConfig{}, apperr.New(apperr.CodeInvalidArgument, "MCP Server %s 请求头解密失败", row.ID)
	}
	return mcp.ServerConfig{
		ID: row.ID, Name: row.Name, Transport: mcp.Transport(row.Transport),
		Command: row.Command, Args: row.Args, Env: env, Cwd: row.Cwd,
		URL: row.URL, Headers: headers, Enabled: row.Enabled,
		TimeoutMS: row.TimeoutMS, MaxOutput: row.MaxOutput,
		DefaultRisk: row.DefaultRisk, AllowTools: row.AllowTools, DenyTools: row.DenyTools,
		HealthIntervalS: row.HealthIntervalS,
	}, nil
}

// Start 启动并注册（幂等：已运行先重建连接）。
func (s *MCPService) Start(ctx context.Context, id string) error {
	row, err := s.main.MCPServers().Get(ctx, id)
	if err != nil {
		return err
	}
	if row == nil {
		return apperr.New(apperr.CodeNotFound, "MCP Server 不存在")
	}
	if !row.Enabled {
		return apperr.New(apperr.CodeInvalidArgument, "MCP Server 已禁用，请先 enable")
	}
	cfg, err := s.toConfig(row)
	if err != nil {
		return err
	}
	// 已在运行 → 原子重载（不产生服务空窗）；未运行 → 直接启动
	if _, running := s.mgr.Handle(id); running {
		if err := s.mgr.Reload(ctx, cfg); err != nil {
			return err
		}
	} else if err := s.mgr.Start(ctx, cfg); err != nil {
		return err
	}
	s.register(id)
	return nil
}

// register 把运行中的 Server 注册进统一注册表（与内置插件同构）。
func (s *MCPService) register(id string) {
	h, ok := s.mgr.Handle(id)
	if !ok {
		return
	}
	// 幂等：重注册前先注销旧条目（RegisterPlugin 遇重复工具名会拒绝）
	if err := s.reg.UnregisterPlugin(s.PluginIDOf(id)); err != nil {
		s.log().Warn("注销旧 MCP 工具失败", "id", id, "err", err)
	}
	if err := s.reg.RegisterPlugin(context.Background(), h.Manifest(), h); err != nil {
		s.log().Error("MCP Server 工具注册失败", "id", id, "err", err)
	}
}

// Disable 停用：注销工具 + 关连接 + 翻转配置（设计 §4.4）。
func (s *MCPService) Disable(ctx context.Context, id string) error {
	if err := s.setToggle(ctx, id, false); err != nil {
		return err
	}
	if err := s.mgr.Stop(id); err != nil {
		return err
	}
	return s.reg.UnregisterPlugin(s.PluginIDOf(id))
}

// Enable 启用（翻转配置位 + 立即启动注册）。
func (s *MCPService) Enable(ctx context.Context, id string) error {
	if err := s.setToggle(ctx, id, true); err != nil {
		return err
	}
	return s.Start(ctx, id)
}

func (s *MCPService) setToggle(ctx context.Context, id string, enabled bool) error {
	row, err := s.main.MCPServers().Get(ctx, id)
	if err != nil {
		return err
	}
	if row == nil {
		return apperr.New(apperr.CodeNotFound, "MCP Server 不存在")
	}
	row.Enabled = enabled
	return s.main.MCPServers().Upsert(ctx, row)
}

// Delete 删除：先注销工具，再关连接，再删配置。
func (s *MCPService) Delete(ctx context.Context, id string) error {
	row, err := s.main.MCPServers().Get(ctx, id)
	if err != nil {
		return err
	}
	if row == nil {
		return apperr.New(apperr.CodeNotFound, "MCP Server 不存在")
	}
	_ = s.reg.UnregisterPlugin(s.PluginIDOf(id))
	_ = s.mgr.Stop(id)
	return s.main.MCPServers().Delete(ctx, id)
}

// Reload 热重载（原子替换；新连接失败保留旧连接，设计 §4.3）。
func (s *MCPService) Reload(ctx context.Context, id string) error {
	row, err := s.main.MCPServers().Get(ctx, id)
	if err != nil {
		return err
	}
	if row == nil {
		return apperr.New(apperr.CodeNotFound, "MCP Server 不存在")
	}
	cfg, err := s.toConfig(row)
	if err != nil {
		return err
	}
	if _, running := s.mgr.Handle(id); running {
		if err := s.mgr.Reload(ctx, cfg); err != nil {
			return err
		}
	} else if err := s.mgr.Start(ctx, cfg); err != nil {
		return err
	}
	s.register(id)
	return nil
}

// Test 联通测试（配置可未落库）：可读错误 + 工具清单 + 耗时。
func (s *MCPService) Test(ctx context.Context, in MCPCreateInput) (*mcp.TestResult, error) {
	cfg, err := s.rowFromInput("", in)
	if err != nil {
		return nil, err
	}
	runCfg, err := s.toConfig(cfg)
	if err != nil {
		return nil, err
	}
	return s.mgr.Test(ctx, runCfg)
}

// Health 健康详情。
func (s *MCPService) Health(ctx context.Context, id string) (*mcp.Status, error) {
	for _, st := range s.mgr.Statuses() {
		if st.ID == id {
			return &st, nil
		}
	}
	return nil, apperr.New(apperr.CodeNotFound, "MCP Server 未运行或不存在")
}

// StartAll 启动期批量拉起 enabled 配置（失败隔离：单个失败仅记录，不阻断启动）。
func (s *MCPService) StartAll(ctx context.Context) {
	rows, err := s.main.MCPServers().List(ctx)
	if err != nil {
		s.log().Warn("读取 MCP Server 配置失败（跳过自动启动）", "err", err)
		return
	}
	started := 0
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		if err := s.Start(ctx, row.ID); err != nil {
			s.log().Warn("MCP Server 启动失败（已隔离，不影响其他 Server）",
				"id", row.ID, "name", row.Name, "err", err)
			continue
		}
		started++
	}
	if started > 0 {
		s.log().Info("已启动 MCP Server", "count", started)
	}
}

// PluginIDOf 暴露插件 ID 规则（注销路径需要）。
func (s *MCPService) PluginIDOf(id string) string { return "mcp:" + id }

func (s *MCPService) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}
