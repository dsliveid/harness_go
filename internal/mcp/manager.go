package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"harness-go/pkg/toolapi"
)

// ServerState Server 生命周期状态（docs/MCP接入设计.md §4）。
type ServerState string

const (
	// StateReady 正常可用。
	StateReady ServerState = "ready"
	// StateDegraded 连续失败熔断（工具移出注入集合，仍可执行——宽进严出）。
	StateDegraded ServerState = "degraded"
	// StateFailed 连接级故障（握手/进程退出且重建失败）。
	StateFailed ServerState = "failed"
	// StateDisabled 已停用。
	StateDisabled ServerState = "disabled"
)

// 熔断阈值（docs/MCP接入设计.md §6：连续失败 ≥3 → degraded）。
const degradeThreshold = 3

// Manager 多 Server 生命周期管理器：启动/停止/重载/联通测试/健康熔断。
//
// 失败隔离（M2）：一个 Server 的连接/协议/进程故障只影响自身状态，
// 绝不阻断其他 Server，更不阻断内核对话链路。
type Manager struct {
	opts Options
	dep  DeploymentPolicy

	mu      sync.Mutex
	servers map[string]*serverHandle
}

// Options 管理器依赖。
type Options struct {
	ClientName    string
	ClientVersion string
	Logger        *slog.Logger
}

// NewManager 创建管理器。
func NewManager(opts Options, dep DeploymentPolicy) *Manager {
	return &Manager{opts: opts, dep: dep, servers: make(map[string]*serverHandle)}
}

// serverHandle 单 Server 运行态。
type serverHandle struct {
	cfg     ServerConfig
	c       *conn
	state   ServerState
	lastErr string
	// 统计（M6 可观测：调用数/失败数）
	calls    int64
	failures int64
	// 连续失败计数（熔断判据；成功即清零）
	consecFails int
	// 包装后的统一工具（与 handle 同生命周期）
	manifest toolapi.Manifest
	tools    []toolapi.Tool
	// 主动健康
	healthStop chan struct{}
	healthOnce sync.Once
}

// TestResult 联通测试结果（设计 §9：返回清单与耗时，清理临时连接）。
type TestResult struct {
	ToolNames  []string `json:"tool_names"`
	ElapsedMS  int64    `json:"elapsed_ms"`
	ServerInfo string   `json:"server_info,omitempty"`
}

// Start 启动（连接 → 握手 → tools/list → 构建统一工具）。
//
// 失败返回错误但不 panic 不阻塞其他 Server（由调用方决定记录方式）。
func (m *Manager) Start(ctx context.Context, cfg ServerConfig) error {
	cfg = cfg.Normalize()
	if err := cfg.Validate(m.dep); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, exists := m.servers[cfg.ID]; exists {
		// Disabled/Failed 的旧句柄可被重建覆盖（卸载重装/启停/重启恢复路径）；
		// 运行中（ready/degraded）才拒绝——那是 Reload 的职责。
		if old.state == StateReady || old.state == StateDegraded {
			return fmt.Errorf("mcp: server %s 已在运行（请先 Stop 或用 Reload）", cfg.ID)
		}
		old.healthStopOnce()
		if old.c != nil {
			_ = old.c.close()
		}
	}
	h, err := m.startLocked(ctx, cfg)
	if err != nil {
		// 失败句柄也要登记（状态 failed，可观测 M6），但不暴露工具
		h.state = StateFailed
		h.lastErr = err.Error()
		m.servers[cfg.ID] = h
		return err
	}
	m.servers[cfg.ID] = h
	return nil
}

// startLocked 建立连接并构建工具（mu 已持有）。
func (m *Manager) startLocked(ctx context.Context, cfg ServerConfig) (*serverHandle, error) {
	h := &serverHandle{cfg: cfg, state: StateFailed}
	c, err := dial(ctx, cfg, m.opts.ClientName, m.opts.ClientVersion)
	if err != nil {
		return h, err
	}
	h.c = c
	raws, lerr := c.listTools(ctx)
	if lerr != nil {
		_ = c.close()
		return h, lerr
	}
	if len(raws) == 0 {
		_ = c.close()
		return h, newError(ErrCodeProtocolError, cfg.ID, "工具清单为空（Server 未暴露任何工具）")
	}
	prefix, namespace, nameKey := m.naming(cfg)
	// 白名单/黑名单过滤（deny 优先于 allow）
	tools := make([]toolapi.Tool, 0, len(raws))
	specs := make([]toolapi.Spec, 0, len(raws))
	for _, raw := range raws {
		if !toolAllowed(cfg, raw.Name) {
			continue
		}
		schemaJSON, _ := json.Marshal(raw.InputSchema) // SDK 形状 any → JSON；nil 落 "null" 由 buildSpec 兜底
		spec := buildSpec(prefix, namespace, nameKey, raw.Name, raw.Description, string(schemaJSON), cfg.DefaultRisk)
		tools = append(tools, &Tool{mgr: m, serverID: cfg.ID, rawName: raw.Name, spec: spec})
		specs = append(specs, spec)
	}
	if len(tools) == 0 {
		_ = c.close()
		return h, newError(ErrCodeProtocolError, cfg.ID, "工具清单为空（白名单/黑名单过滤后无可用工具）")
	}
	h.tools = tools
	h.manifest = toolapi.Manifest{
		ID:              m.pluginID(cfg),
		Name:            cfg.Name,
		Version:         "1.0.0",
		Kind:            m.kind(cfg),
		Description:     m.pluginKindLabel(cfg) + cfg.Name,
		DefaultTier:     toolapi.TierDeferred,
		ProtocolVersion: toolapi.ProtocolVersion,
		Tools:           specs,
	}
	h.state = StateReady
	m.startHealthLocked(h)
	return h, nil
}

// naming 前缀、命名空间与工具名段（MCP Server 与本地工具包共用一套连接代码路径，
// 仅命名前缀与命名空间不同——「只差包从哪来」）。
// 工具名段：包用稳定 ID（安装即定型，升级/回滚不变），MCP Server 用展示名规范化。
func (m *Manager) naming(cfg ServerConfig) (prefix, namespace, nameKey string) {
	if cfg.PluginKind == toolapi.KindPackage {
		return "pkg", "pkg:" + cfg.ID, ServerKey(cfg.ID)
	}
	return "mcp", "mcp:" + cfg.ID, ServerKey(cfg.Name)
}

// kind 插件形态。
func (m *Manager) kind(cfg ServerConfig) toolapi.Kind {
	if cfg.PluginKind == toolapi.KindPackage {
		return toolapi.KindPackage
	}
	return toolapi.KindMCP
}

// pluginKindLabel manifest 描述前缀。
func (m *Manager) pluginKindLabel(cfg ServerConfig) string {
	if cfg.PluginKind == toolapi.KindPackage {
		return "本地工具包: "
	}
	return "MCP Server: "
}

// PluginID 注册表插件 ID。
func (m *Manager) pluginID(cfg ServerConfig) string {
	if cfg.PluginKind == toolapi.KindPackage {
		return "pkg:" + cfg.ID
	}
	return "mcp:" + cfg.ID
}

// toolAllowed 黑名单优先的白名单过滤。
func toolAllowed(cfg ServerConfig, rawName string) bool {
	for _, d := range cfg.DenyTools {
		if d == rawName {
			return false
		}
	}
	if len(cfg.AllowTools) == 0 {
		return true
	}
	for _, a := range cfg.AllowTools {
		if a == rawName {
			return true
		}
	}
	return false
}

// Stop 停用并注销（设计 §4.4）。
func (m *Manager) Stop(serverID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.servers[serverID]
	if !ok {
		return nil // 未启动即视为已停（幂等）
	}
	h.healthStopOnce()
	if h.c != nil {
		_ = h.c.close()
	}
	h.state = StateDisabled
	h.tools = nil
	return nil
}

// StopAll 全部停用（进程退出时调用）。
func (m *Manager) StopAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.servers))
	for id := range m.servers {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.Stop(id)
	}
}

// Reload 热重载：新配置先建连成功，再原子替换（设计 §4.3）。
//
// 新连接失败时保留旧连接不动并返回错误（不产生服务空窗）。
func (m *Manager) Reload(ctx context.Context, cfg ServerConfig) error {
	cfg = cfg.Normalize()
	if err := cfg.Validate(m.dep); err != nil {
		return err
	}
	m.mu.Lock()
	old := m.servers[cfg.ID]
	m.mu.Unlock()

	h, err := m.startLocked(ctx, cfg)
	if err != nil {
		return err // 旧连接未动
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old != nil {
		old.healthStopOnce()
		if old.c != nil {
			_ = old.c.close()
		}
	}
	m.servers[cfg.ID] = h
	return nil
}

// Test 联通测试（配置尚未落库也可用）：建连 → 握手 → tools/list → 清理。
// 错误信息保持可读：命令不存在 / 连接超时 / 协议错误 / 空清单各自有明确文案。
func (m *Manager) Test(ctx context.Context, cfg ServerConfig) (*TestResult, error) {
	cfg = cfg.Normalize()
	if err := cfg.Validate(m.dep); err != nil {
		return nil, err
	}
	begin := time.Now()
	c, err := dial(ctx, cfg, m.opts.ClientName, m.opts.ClientVersion)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.close() }()
	raws, err := c.listTools(ctx)
	if err != nil {
		return nil, err
	}
	if len(raws) == 0 {
		return nil, newError(ErrCodeProtocolError, cfg.ID, "连接成功但工具清单为空（Server 未暴露任何工具）")
	}
	names := make([]string, 0, len(raws))
	for _, r := range raws {
		names = append(names, r.Name)
	}
	info := ""
	if ir := c.session.InitializeResult(); ir != nil && ir.ServerInfo != nil {
		info = ir.ServerInfo.Name + " " + ir.ServerInfo.Version
	}
	return &TestResult{
		ToolNames:  names,
		ElapsedMS:  time.Since(begin).Milliseconds(),
		ServerInfo: info,
	}, nil
}

// CallTool 执行单次工具调用（工具包装器 Invoke 的路由终点）。
//
// 失败计数与熔断：连续 ≥3 次失败 → degraded（移出注入集合）；
// stdio 进程退出 → 尝试重建一次（被动自愈），仍失败归一 MCP_PROCESS_EXIT。
func (m *Manager) CallTool(ctx context.Context, serverID, rawName string, args []byte) (string, bool, error) {
	m.mu.Lock()
	h, ok := m.servers[serverID]
	if !ok || h.state == StateDisabled {
		m.mu.Unlock()
		return "", false, newError(ErrCodeDisabled, serverID, "Server 未启动或已停用")
	}
	if h.state == StateFailed {
		m.mu.Unlock()
		return "", false, newError(ErrCodeConnectFailed, serverID, "Server 连接失败: %s", h.lastErr)
	}
	// stdio 进程退出：被动重建一次（设计 §6 传输层重建）
	if h.c != nil && h.c.exited() {
		m.opts.log().Warn("stdio 进程已退出，尝试重建连接", "server", serverID)
		nh, err := m.startLocked(ctx, h.cfg)
		if err != nil {
			h.state = StateFailed
			h.lastErr = err.Error()
			m.mu.Unlock()
			return "", false, newError(ErrCodeProcessExit, serverID, "进程已退出且重建失败: %v", err)
		}
		h.c = nh.c
		h.state = StateReady
		h.consecFails = 0
	}
	c := h.c
	h.calls++
	m.mu.Unlock()

	text, isErr, err := c.callTool(ctx, rawName, args)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		h.failures++
		h.consecFails++
		if h.consecFails >= degradeThreshold && h.state == StateReady {
			h.state = StateDegraded
			m.opts.log().Warn("连续失败，Server 熔断（工具移出注入集合）",
				"server", serverID, "consecutive_failures", h.consecFails)
		}
		return "", false, err
	}
	if isErr {
		// 工具级错误：计失败但语义是「模型可自我纠正」（不熔断连接本身）
		h.failures++
		return text, true, nil
	}
	h.consecFails = 0
	if h.state == StateDegraded {
		h.state = StateReady // 探测恢复
		m.opts.log().Info("Server 恢复正常", "server", serverID)
	}
	return text, false, nil
}

// Handle 返回某 Server 的运行句柄（service 层注册进 Registry 用）。
func (m *Manager) Handle(serverID string) (*Handle, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[serverID]; !ok {
		return nil, false
	}
	return &Handle{m: m, id: serverID}, true
}

// Handle 一个已启动 Server 的 toolapi.Plugin 视图（注册进统一 Registry）。
type Handle struct {
	m  *Manager
	id string
}

// Manifest 实现 toolapi.Plugin。
func (h *Handle) Manifest() toolapi.Manifest {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.servers[h.id].manifest
}

// Tools 实现 toolapi.Plugin（返回包装后的统一工具）。
func (h *Handle) Tools() []toolapi.Tool {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.servers[h.id].tools
}

// Health 实现 toolapi.Plugin。
func (h *Handle) Health() toolapi.Health {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	st := toolapi.HealthAvailable
	switch h.m.servers[h.id].state {
	case StateDegraded:
		st = toolapi.HealthDegraded
	case StateFailed:
		st = toolapi.HealthUnavailable
	}
	return toolapi.Health{Status: st, Message: h.m.servers[h.id].lastErr, CheckedAt: time.Now()}
}

// Start 实现 toolapi.Plugin（生命周期由 Manager 管控，此处幂等空操作）。
func (h *Handle) Start(context.Context) error { return nil }

// Stop 实现 toolapi.Plugin（Registry.Stop / 卸载路径）。
func (h *Handle) Stop(context.Context) error { return h.m.Stop(h.id) }

// Status 全量状态（API 响应）。
type Status struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Transport string `json:"transport"`
	State     string `json:"state"`
	LastError string `json:"last_error,omitempty"`
	Calls     int64  `json:"calls"`
	Failures  int64  `json:"failures"`
	ToolCount int    `json:"tool_count"`
}

// Statuses 全部 Server 状态快照。
func (m *Manager) Statuses() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Status, 0, len(m.servers))
	for _, h := range m.servers {
		st := Status{
			ID:        h.cfg.ID,
			Name:      h.cfg.Name,
			Transport: string(h.cfg.Transport),
			State:     string(h.state),
			LastError: h.lastErr,
			Calls:     h.calls,
			Failures:  h.failures,
		}
		if h.state == StateReady || h.state == StateDegraded {
			st.ToolCount = len(h.tools)
		}
		out = append(out, st)
	}
	return out
}

// State 单 Server 状态查询。
func (m *Manager) State(serverID string) (ServerState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.servers[serverID]
	if !ok {
		return "", errors.New("mcp: server 不存在")
	}
	return h.state, nil
}

// startHealthLocked 主动健康探针（health_interval_s > 0 时启用）：
// 定期 ping，连续 2 次失败 → degraded；成功即恢复（设计 §6）。
func (m *Manager) startHealthLocked(h *serverHandle) {
	if h.cfg.HealthIntervalS <= 0 {
		return
	}
	h.healthStop = make(chan struct{})
	stop := h.healthStop
	go func() {
		ticker := time.NewTicker(time.Duration(h.cfg.HealthIntervalS) * time.Second)
		defer ticker.Stop()
		pingFails := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				m.mu.Lock()
				c := h.c
				if c == nil {
					m.mu.Unlock()
					return
				}
				m.mu.Unlock()
				if err := c.ping(context.Background()); err != nil {
					pingFails++
					if pingFails >= 2 {
						m.mu.Lock()
						if h.state == StateReady {
							h.state = StateDegraded
							h.lastErr = "健康检查失败: " + err.Error()
						}
						m.mu.Unlock()
					}
					continue
				}
				pingFails = 0
				m.mu.Lock()
				if h.state == StateDegraded && h.consecFails == 0 {
					h.state = StateReady
					h.lastErr = ""
				}
				m.mu.Unlock()
			}
		}
	}()
}

// healthStopOnce 停止健康探针（幂等）。
func (h *serverHandle) healthStopOnce() {
	h.healthOnce.Do(func() {
		if h.healthStop != nil {
			close(h.healthStop)
		}
	})
}

func (o Options) log() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

// 编译期保证：Handle 满足插件契约（与内置插件同构注册）。
var _ toolapi.Plugin = (*Handle)(nil)
var _ = sdkmcp.ClientSession{}
