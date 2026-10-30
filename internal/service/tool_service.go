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
