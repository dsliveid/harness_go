// Package registry 维护工具注册表：注册/冲突检测/解析/tier 三层覆盖/软上限告警/成本统计。
//
// 铁律（D11）：
//   - tier 只决定**注入深度**，不决定能否执行（宽进严出）；
//   - `core` / `deferred` 的条数是**软上限**，超限只告警、绝不阻断启动。
package registry

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"harness-go/pkg/toolapi"
)

// 默认软上限（仅告警）。
const (
	// DefaultCoreSoftLimit 基础工具条数软上限。
	DefaultCoreSoftLimit = 8
	// DefaultDeferredSoftLimit 按需工具条数软上限。
	DefaultDeferredSoftLimit = 60
)

// Options 注册表选项。
type Options struct {
	CoreSoftLimit     int
	DeferredSoftLimit int
	Logger            *slog.Logger
	Now               func() time.Time
}

func (o Options) normalize() Options {
	out := o
	if out.CoreSoftLimit <= 0 {
		out.CoreSoftLimit = DefaultCoreSoftLimit
	}
	if out.DeferredSoftLimit <= 0 {
		out.DeferredSoftLimit = DefaultDeferredSoftLimit
	}
	if out.Now == nil {
		out.Now = time.Now
	}
	return out
}

// Limits 软上限。
type Limits struct {
	CoreSoft     int `json:"core_soft"`
	DeferredSoft int `json:"deferred_soft"`
}

// SpecSummary 单工具摘要（对外端点与启动日志用）。
type SpecSummary struct {
	Name        string           `json:"name"`
	Namespace   string           `json:"namespace"`
	Tier        toolapi.Tier     `json:"tier"`         // 生效档位（已应用覆盖）
	DefaultTier toolapi.Tier     `json:"default_tier"` // 插件声明档位
	CostTokens  int              `json:"cost_tokens"`
	Risk        toolapi.Risk     `json:"risk"`
	Executor    toolapi.Executor `json:"executor"`
	PluginID    string           `json:"plugin_id"`
	Health      toolapi.Health   `json:"health"`
}

// PluginInfo 插件摘要。
type PluginInfo struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Kind        string   `json:"kind"`
	DefaultTier string   `json:"default_tier"`
	Tools       []string `json:"tools"`
}

// Snapshot 注册表一致性快照（只读副本，可安全并发使用）。
type Snapshot struct {
	Total        int                     `json:"total"`
	Core         []SpecSummary           `json:"core"`
	Deferred     []SpecSummary           `json:"deferred"`
	OnDemand     []SpecSummary           `json:"on_demand"`
	CostTotal    int                     `json:"cost_total"`
	CostCore     int                     `json:"cost_core"`
	CostDeferred int                     `json:"cost_deferred"`
	Limits       Limits                  `json:"limits"`
	Warnings     []string                `json:"warnings"`
	Plugins      []PluginInfo            `json:"plugins"`
	Overrides    map[string]toolapi.Tier `json:"overrides"`
	GeneratedAt  time.Time               `json:"generated_at"`
}

// entry 单个已注册工具。
type entry struct {
	pluginID    string
	tool        toolapi.Tool
	spec        toolapi.Spec // 插件声明（Name/Tier/Risk/Cost 已归一）
	defaultTier toolapi.Tier
	health      toolapi.Health
}

// pluginEntry 已启动的插件。
type pluginEntry struct {
	manifest toolapi.Manifest
	plugin   toolapi.Plugin
	tools    []string
}

// Registry 工具注册表（并发安全）。
type Registry struct {
	mu        sync.RWMutex
	opts      Options
	tools     map[string]*entry
	order     []string
	plugins   map[string]*pluginEntry
	overrides map[string]toolapi.Tier
	warnings  []string
}

// New 创建空注册表。
func New(opts Options) *Registry {
	return &Registry{
		opts:      opts.normalize(),
		tools:     make(map[string]*entry),
		plugins:   make(map[string]*pluginEntry),
		overrides: make(map[string]toolapi.Tier),
	}
}

// RegisterFactory 装配插件：Manifest 校验 → New → Start → RegisterPlugin。
//
// 失败即返回错误（启动期），由 cmd/harness-go/plugins.go 决定是否致命。
func (r *Registry) RegisterFactory(ctx context.Context, f toolapi.Factory, host toolapi.Host, cfg toolapi.Config) error {
	if f == nil {
		return fmt.Errorf("插件工厂为空")
	}
	m := f.Manifest()
	if err := m.Validate(); err != nil {
		return fmt.Errorf("插件清单校验失败: %w", err)
	}
	p, err := f.New(host, cfg)
	if err != nil {
		return fmt.Errorf("插件 %s 初始化失败: %w", m.ID, err)
	}
	if p == nil {
		return fmt.Errorf("插件 %s 返回了空实例", m.ID)
	}
	if err := p.Start(ctx); err != nil {
		return fmt.Errorf("插件 %s 启动失败: %w", m.ID, err)
	}
	return r.RegisterPlugin(ctx, m, p)
}

// RegisterPlugin 注册已启动的插件（工具名冲突即拒绝整插件）。
func (r *Registry) RegisterPlugin(_ context.Context, m toolapi.Manifest, p toolapi.Plugin) error {
	if err := m.Validate(); err != nil {
		return err
	}
	tools := p.Tools()
	if len(tools) == 0 {
		return fmt.Errorf("插件 %s 未暴露任何工具", m.ID)
	}

	// 先在副本上校验，全部通过才提交（避免半注册状态）
	r.mu.RLock()
	seen := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		s := t.Spec().Normalized()
		if _, ok := r.tools[s.Name]; ok {
			r.mu.RUnlock()
			return fmt.Errorf("%w: %s（已被其他插件注册）", toolapi.ErrDuplicate, s.Name)
		}
		seen[s.Name] = struct{}{}
	}
	r.mu.RUnlock()

	if err := m.Validate(); err != nil {
		return err
	}
	if len(m.Tools) > 0 {
		for _, decl := range m.Tools {
			if _, ok := seen[decl.Normalized().Name]; !ok {
				return fmt.Errorf("插件 %s 清单声明了工具 %s，但运行时未暴露", m.ID, decl.Name)
			}
		}
	}

	health := p.Health()
	if health.Status == "" {
		health = toolapi.Health{Status: toolapi.HealthAvailable}
	}
	if health.CheckedAt.IsZero() {
		health.CheckedAt = r.opts.Now()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range tools {
		s := t.Spec().Normalized()
		if s.Tier == "" {
			s.Tier = m.DefaultTier
		}
		if s.Tier == "" {
			s.Tier = toolapi.TierDeferred
		}
		if _, ok := r.tools[s.Name]; ok {
			return fmt.Errorf("%w: %s", toolapi.ErrDuplicate, s.Name)
		}
		r.tools[s.Name] = &entry{pluginID: m.ID, tool: t, spec: s, defaultTier: s.Tier, health: health}
		r.order = append(r.order, s.Name)
	}
	r.plugins[m.ID] = &pluginEntry{manifest: m, plugin: p, tools: toolNames(tools)}
	r.refreshWarningsLocked()
	return nil
}

// RegisterTool 直接注册单个工具（fixture / 单测便利入口）。
func (r *Registry) RegisterTool(t toolapi.Tool, pluginID string) error {
	s := t.Spec().Normalized()
	if err := s.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[s.Name]; ok {
		return fmt.Errorf("%w: %s", toolapi.ErrDuplicate, s.Name)
	}
	if pluginID == "" {
		pluginID = s.Namespace
	}
	r.tools[s.Name] = &entry{pluginID: pluginID, tool: t, spec: s, defaultTier: s.Tier,
		health: toolapi.Health{Status: toolapi.HealthAvailable, CheckedAt: r.opts.Now()}}
	r.order = append(r.order, s.Name)
	r.refreshWarningsLocked()
	return nil
}

// Resolve 解析工具（返回的 Spec 已应用 tier 覆盖；未注册返回 ErrNotFound）。
func (r *Registry) Resolve(name string) (toolapi.Tool, toolapi.Spec, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.tools[name]
	if !ok {
		return nil, toolapi.Spec{}, fmt.Errorf("%w: %s", toolapi.ErrNotFound, name)
	}
	spec := e.spec
	spec.Tier = r.tierLocked(e)
	return e.tool, spec, nil
}

// Risk 返回工具风险级（未注册时 ok=false，由调用方保守处理）。
func (r *Registry) Risk(name string) (toolapi.Risk, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.tools[name]
	if !ok {
		return "", false
	}
	return e.spec.Risk, true
}

// Specs 返回全部工具规格（tier 已应用覆盖，按注册顺序）。
func (r *Registry) Specs() []toolapi.Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]toolapi.Spec, 0, len(r.order))
	for _, name := range r.order {
		e := r.tools[name]
		s := e.spec
		s.Tier = r.tierLocked(e)
		out = append(out, s)
	}
	return out
}

// UnregisterPlugin 注销插件及其全部工具（阶段 E：卸载/禁用/重载路径）。
//
// 语义：正在执行的调用允许完成（调用方持有 Tool 引用）；
// 注销后 Resolve/Risk 立即 miss，注入集合在下一个 Run 生效（保护前缀缓存）。
// 插件不存在的视为已注销（幂等，返回 nil）。
func (r *Registry) UnregisterPlugin(pluginID string) error {
	r.mu.Lock()
	pe, ok := r.plugins[pluginID]
	if !ok {
		r.mu.Unlock()
		return nil
	}
	removed := make(map[string]struct{}, len(pe.tools))
	for _, name := range pe.tools {
		removed[name] = struct{}{}
		delete(r.tools, name)
	}
	delete(r.plugins, pluginID)
	kept := r.order[:0]
	for _, name := range r.order {
		if _, gone := removed[name]; gone {
			continue
		}
		kept = append(kept, name)
	}
	r.order = kept
	r.refreshWarningsLocked()
	r.mu.Unlock()
	// 在锁外执行 Stop：插件可能做 IO，避免长时间持锁
	_ = pe.plugin.Stop(context.Background())
	return nil
}

// SetPluginHealth 更新插件健康态（阶段 E：MCP 熔断/恢复联动注入集合）。
//
// unavailable / degraded 的插件工具被移出**注入集合**（InjectedSpecs），
// 但仍注册、仍可被显式调用（宽进严出：熔断≠禁用）。
func (r *Registry) SetPluginHealth(pluginID string, h toolapi.Health) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h.CheckedAt.IsZero() {
		h.CheckedAt = r.opts.Now()
	}
	for _, e := range r.tools {
		if e.pluginID == pluginID {
			e.health = h
		}
	}
}

// PluginHealth 返回插件健康态快照（未注册返回 unavailable）。
func (r *Registry) PluginHealth(pluginID string) toolapi.Health {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.tools {
		if e.pluginID == pluginID {
			return e.health
		}
	}
	return toolapi.Health{Status: toolapi.HealthUnavailable}
}

// InjectedSpecs 返回**当前应注入**的工具规格：health 为 unavailable/degraded
// 的插件（如 MCP Server 熔断）整体移出注入集合（docs/MCP接入设计.md §6）。
// 已注册但被移出的工具仍可被调用（Resolve 不受影响——宽进严出）。
func (r *Registry) InjectedSpecs() []toolapi.Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]toolapi.Spec, 0, len(r.order))
	for _, name := range r.order {
		e := r.tools[name]
		if e.health.Status == toolapi.HealthUnavailable || e.health.Status == toolapi.HealthDegraded {
			continue
		}
		s := e.spec
		s.Tier = r.tierLocked(e)
		out = append(out, s)
	}
	return out
}

// SetOverrides 设置 tier 覆盖（来自主库 settings.tools.tiers，仅允许已知工具名）。
func (r *Registry) SetOverrides(over map[string]toolapi.Tier) {
	clean := make(map[string]toolapi.Tier, len(over))
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, tier := range over {
		if _, ok := r.tools[name]; !ok {
			continue // 未注册工具名静默忽略（工具可能尚未安装）
		}
		if !tier.Valid() {
			continue
		}
		clean[name] = tier
	}
	r.overrides = clean
	r.refreshWarningsLocked()
}

// Overrides 返回当前覆盖副本。
func (r *Registry) Overrides() map[string]toolapi.Tier {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]toolapi.Tier, len(r.overrides))
	for k, v := range r.overrides {
		out[k] = v
	}
	return out
}

// Snapshot 返回一致性快照。
func (r *Registry) Snapshot() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	snap := Snapshot{
		Total:       len(r.order),
		Limits:      Limits{CoreSoft: r.opts.CoreSoftLimit, DeferredSoft: r.opts.DeferredSoftLimit},
		Warnings:    append([]string(nil), r.warnings...),
		Overrides:   make(map[string]toolapi.Tier, len(r.overrides)),
		GeneratedAt: r.opts.Now(),
	}
	for k, v := range r.overrides {
		snap.Overrides[k] = v
	}
	for _, name := range r.order {
		e := r.tools[name]
		s := e.spec
		s.Tier = r.tierLocked(e)
		sum := SpecSummary{
			Name: s.Name, Namespace: s.Namespace, Tier: s.Tier, DefaultTier: e.defaultTier,
			CostTokens: s.CostTokens, Risk: s.Risk, Executor: s.Executor,
			PluginID: e.pluginID, Health: e.health,
		}
		switch s.Tier {
		case toolapi.TierCore:
			snap.Core = append(snap.Core, sum)
			snap.CostCore += s.CostTokens
		case toolapi.TierOnDemand:
			snap.OnDemand = append(snap.OnDemand, sum)
		default:
			snap.Deferred = append(snap.Deferred, sum)
			snap.CostDeferred += s.CostTokens
		}
		snap.CostTotal += s.CostTokens
	}
	for _, pe := range r.plugins {
		snap.Plugins = append(snap.Plugins, PluginInfo{
			ID: pe.manifest.ID, Name: pe.manifest.Name, Version: pe.manifest.Version,
			Kind: string(pe.manifest.Kind), DefaultTier: string(pe.manifest.DefaultTier),
			Tools: append([]string(nil), pe.tools...),
		})
	}
	sort.Slice(snap.Plugins, func(i, j int) bool { return snap.Plugins[i].ID < snap.Plugins[j].ID })
	return snap
}

// Warnings 返回软上限告警（仅告警，不影响可用性）。
func (r *Registry) Warnings() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.warnings...)
}

// Stop 停止所有插件（幂等，优雅关闭时调用）。
func (r *Registry) Stop(ctx context.Context) {
	r.mu.Lock()
	plugins := make([]*pluginEntry, 0, len(r.plugins))
	for _, pe := range r.plugins {
		plugins = append(plugins, pe)
	}
	r.mu.Unlock()
	for _, pe := range plugins {
		if err := pe.plugin.Stop(ctx); err != nil && r.opts.Logger != nil {
			r.opts.Logger.Warn("插件停止失败", "plugin", pe.manifest.ID, "err", err)
		}
	}
}

// tierLocked 计算生效档位（优先级：全局覆盖 > 插件声明）。
func (r *Registry) tierLocked(e *entry) toolapi.Tier {
	if t, ok := r.overrides[e.spec.Name]; ok {
		return t
	}
	if e.spec.Tier != "" {
		return e.spec.Tier
	}
	return toolapi.TierDeferred
}

// refreshWarningsLocked 重算软上限告警。
func (r *Registry) refreshWarningsLocked() {
	var core, deferred []string
	for _, name := range r.order {
		e := r.tools[name]
		switch r.tierLocked(e) {
		case toolapi.TierCore:
			core = append(core, name)
		case toolapi.TierDeferred:
			deferred = append(deferred, name)
		}
	}
	var warns []string
	if len(core) > r.opts.CoreSoftLimit {
		warns = append(warns, fmt.Sprintf("core 档工具 %d 条，超过软上限 %d（仅告警，不阻断启动）: %v",
			len(core), r.opts.CoreSoftLimit, core))
	}
	if len(deferred) > r.opts.DeferredSoftLimit {
		warns = append(warns, fmt.Sprintf("deferred 档工具 %d 条，超过软上限 %d（仅告警，不阻断启动）",
			len(deferred), r.opts.DeferredSoftLimit))
	}
	r.warnings = warns
	if len(warns) > 0 && r.opts.Logger != nil {
		for _, w := range warns {
			r.opts.Logger.Warn("工具注入预算告警", "detail", w)
		}
	}
}

func toolNames(tools []toolapi.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Spec().Normalized().Name)
	}
	return out
}
