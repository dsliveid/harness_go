package service

import (
	"context"
	"encoding/json"
	"sort"

	"harness-go/internal/store"
	"harness-go/internal/tools"
	"harness-go/internal/tools/loader"
	"harness-go/internal/tools/registry"
	"harness-go/pkg/toolapi"
)

// settingsKeyToolTiers 主库 settings 中「全局 tier 覆盖」的键。
//
// 三层覆盖优先级：插件声明（default_tier） < 本键（全局） < 会话级 session_settings（阶段 D 落地）。
const settingsKeyToolTiers = "tools.tiers"

// settingsKeyToolDisabled 主库 settings 中「已停用工具」的键（JSON 字符串数组）。
//
// 与 tier 的关系：tier = 注入深度（可见性）；disabled = 是否启用（可用性）。
// 两者正交：停用的工具既不注入也不可调用，重新启用后原 tier 原样恢复。
const settingsKeyToolDisabled = "tools.disabled"

// ToolService 工具可观测与注入配置用例。
//
// 只经 tools.Runner 访问注册表，不直接触碰插件实现；写配置落主库 settings。
type ToolService struct {
	runner *tools.Runner
	main   *store.MainStore
}

// NewToolService 创建工具用例。
func NewToolService(runner *tools.Runner, main *store.MainStore) *ToolService {
	return &ToolService{runner: runner, main: main}
}

// List 返回工具注册表快照（tier / default_tier / cost / health）。
func (s *ToolService) List(_ context.Context) registry.Snapshot {
	return s.runner.Snapshot()
}

// Injection 返回本轮注入视图与 token 预算明细。
func (s *ToolService) Injection(_ context.Context) loader.Injection {
	return s.runner.Injection()
}

// SetTiers 更新全局 tier 覆盖：校验 → 持久化主库 → 内存立即生效（下一轮对话）。
func (s *ToolService) SetTiers(ctx context.Context, tiers map[string]string) (registry.Snapshot, error) {
	parsed := make(map[string]toolapi.Tier, len(tiers))
	for name, raw := range tiers {
		tier := toolapi.Tier(raw)
		if !tier.Valid() {
			return registry.Snapshot{}, invalidArg("工具 %s 的档位非法: %q（可选 core/deferred/on_demand）", name, raw)
		}
		parsed[name] = tier
	}
	// 只接受已注册工具，避免写入永不适用的配置
	known := make(map[string]struct{})
	for _, spec := range s.runner.Specs() {
		known[spec.Name] = struct{}{}
	}
	unknown := make([]string, 0)
	for name := range parsed {
		if _, ok := known[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return registry.Snapshot{}, invalidArg("未知工具: %v", unknown)
	}

	payload := make(map[string]string, len(parsed))
	for name, tier := range parsed {
		payload[name] = string(tier)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return registry.Snapshot{}, err
	}
	if err := s.main.Settings().Set(ctx, settingsKeyToolTiers, json.RawMessage(raw)); err != nil {
		return registry.Snapshot{}, err
	}
	s.runner.SetOverrides(parsed)
	return s.runner.Snapshot(), nil
}

// LoadOverrides 启动时从主库加载已持久化的 tier 覆盖（缺失或损坏时静默忽略）。
func (s *ToolService) LoadOverrides(ctx context.Context) {
	raw, err := s.main.Settings().Get(ctx, settingsKeyToolTiers)
	if err != nil || len(raw) == 0 {
		return
	}
	var stored map[string]string
	if uErr := json.Unmarshal(raw, &stored); uErr != nil {
		return
	}
	parsed := make(map[string]toolapi.Tier, len(stored))
	for name, tier := range stored {
		parsed[name] = toolapi.Tier(tier)
	}
	s.runner.SetOverrides(parsed)
}

// SetToolEnabled 启用/停用系统内置工具（服务端自身能力开关）。
//
// 语义：
//   - 停用 = 从注入集合移除 + Resolve 直接失败（模型看不到也调不动）；
//   - 停用**不卸载**：内置工具随程序发布，不存在卸载动作，重新启用即原样恢复；
//   - 与 tier 正交：只翻转可用性，不动注入档位。
func (s *ToolService) SetToolEnabled(ctx context.Context, name string, enabled bool) (registry.Snapshot, error) {
	if name == "" {
		return registry.Snapshot{}, invalidArg("工具名不能为空")
	}
	// 只接受已注册工具，避免写入永不适用的配置（与 SetTiers 同口径）
	known := false
	for _, spec := range s.runner.Specs() {
		if spec.Name == name {
			known = true
			break
		}
	}
	if !known {
		return registry.Snapshot{}, invalidArg("未知工具: %s", name)
	}

	next := make([]string, 0, len(s.runner.Disabled())+1)
	for _, n := range s.runner.Disabled() {
		if n != name {
			next = append(next, n)
		}
	}
	if !enabled {
		next = append(next, name)
	}
	sort.Strings(next)

	raw, err := json.Marshal(next)
	if err != nil {
		return registry.Snapshot{}, err
	}
	if err := s.main.Settings().Set(ctx, settingsKeyToolDisabled, json.RawMessage(raw)); err != nil {
		return registry.Snapshot{}, err
	}
	s.runner.SetDisabled(next)
	return s.runner.Snapshot(), nil
}

// LoadDisabled 启动时从主库加载「已停用工具」清单（缺失或损坏时静默忽略）。
//
// 必须在插件注册完成之后调用：Registry.SetDisabled 会丢弃未注册的工具名，
// 早于注册调用会让持久化配置被静默清空。
func (s *ToolService) LoadDisabled(ctx context.Context) {
	raw, err := s.main.Settings().Get(ctx, settingsKeyToolDisabled)
	if err != nil || len(raw) == 0 {
		return
	}
	var stored []string
	if uErr := json.Unmarshal(raw, &stored); uErr != nil {
		return
	}
	s.runner.SetDisabled(stored)
}
