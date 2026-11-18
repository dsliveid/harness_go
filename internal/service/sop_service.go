package service

import (
	"context"
	"errors"
	"strings"

	"harness-go/internal/sop"
)

// SOPService 核心 SOP（agent-core）+ 原生 SOP 目录的用例层。
//
// 职责边界：
//   - 本层只做**编排**：把 HTTP 语义映射到 internal/sop.Loader / RenderRules，
//     并组装「生效预览」这类跨信息聚合的视图；
//   - 真源解析全部下沉到 internal/sop（零依赖内核层），本层不碰文件 IO 细节；
//   - 不持有任何缓存：每轮对话都重新读盘/重算，保证「开关改完下一轮即生效」（热生效）。
//
// 与 RunService 的协作：
//   - RunService / ChildRunner 通过系统提示装配点调用 SystemBase / Rules
//     （见 main.go 的 RulesFunc 注入），与既有 SOPFunc / ToolRefs 同款模式，不新造机制。
//   - 子 Run 与父 Run 同口径（体感一致），差别只在 Context.IsSub 决定的作用域过滤。
type SOPService struct {
	loader *sop.Loader
	// disabledProvider 读取用户关闭的规则 ID（settings 键 agent.disabled_sops）。
	//
	// 为什么用函数注入而不是构造期传入：每 Run 都要重读，
	// 界面关掉某条规范必须「下一轮即刻生效」，不能在启动期定格。
	// nil 表示用户未关闭任何规范。
	disabledProvider func(ctx context.Context) []string
	// planModeProvider 读取规划模式（standard / always_plan / always_proceed）。
	//
	// 与 disabledProvider 同款：每 Run 重读，切完模式下一轮提示词立刻换措辞。
	// nil 或返回未知值时回落 standard（保守：不强加计划门禁）。
	planModeProvider func(ctx context.Context) string
}

// NewSOPService 创建核心 SOP 用例。
//
// dataDir 为 config.EnsureDataDir 的返回值（绝对路径）；空串时退化为
// 「仅出厂内置」模式（运行时不落盘，读写接口返回明确错误）。
func NewSOPService(dataDir string, budget int) *SOPService {
	return &SOPService{loader: sop.NewLoader(dataDir, budget)}
}

// ErrSOPUnavailable 服务未装配。
var ErrSOPUnavailable = errors.New("service: SOP 服务不可用")

// SetDisabledProvider 注入「用户关闭的规则 ID」读取源（settings 表）。
//
// 允许后置注入，避免构造期循环依赖（SOPService 在 SettingsService 之前装配）。
func (s *SOPService) SetDisabledProvider(f func(ctx context.Context) []string) {
	if s == nil {
		return
	}
	s.disabledProvider = f
}

// SetPlanModeProvider 注入「规划模式」读取源（settings / 项目配置）。
//
// 未注入时回落 standard（不强加计划门禁），保证未装配场景行为与今天一致。
func (s *SOPService) SetPlanModeProvider(f func(ctx context.Context) string) {
	if s == nil {
		return
	}
	s.planModeProvider = f
}

// Core 返回核心 SOP 的生效快照（含来源、token、预算与出厂原文）。
func (s *SOPService) Core(_ context.Context) sop.Core {
	if s == nil || s.loader == nil {
		return sop.Core{}
	}
	return s.loader.LoadCore()
}

// CoreText 返回当前生效的核心 SOP 正文（供 RunService 每 Run 注入 SystemBase）。
//
// 返回空串表示降级（服务未装配），调用方应保留自身默认提示，不得注入空系统提示。
func (s *SOPService) CoreText() string {
	if s == nil || s.loader == nil {
		return ""
	}
	return s.loader.LoadCore().Content
}

// Rules 返回本次上下文实际生效的规则行（含序号，`N. 【标题】：正文`）。
//
// rc.Disabled / rc.PlanMode 由本层从 settings 注入源补齐 —— 调用方（RunService/ChildRunner）
// 不需要知道开关存在哪里，只需如实交代会话事实（IsSub / PlanMode / SessionID）。
func (s *SOPService) Rules(ctx context.Context, rc sop.Context) []string {
	if s == nil {
		// 未装配 = 不注入规则（与 CoreText 返回空串同口径，绝不假装修过）。
		return nil
	}
	rules, _ := sop.RenderRules(s.normalize(ctx, rc), 1)
	return rules
}

// normalize 补齐上下文的外部事实（开关清单 + 规划模式）。
//
// ⚠️ 必须是「返回值」而不是就地改写入参：Context 是值类型，把补齐逻辑
// 分别塞进 Rules / Effective 各自实现过一次，曾导致 /sop/effective 的
// Rules 已过滤、ActiveIDs 却仍按全开计算（界面显示「已关闭但仍在生效」）。
// 现在只有这一处补齐，两处消费同一个补齐后的副本。
func (s *SOPService) normalize(ctx context.Context, rc sop.Context) sop.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	rc.Disabled = s.disabledIDs(ctx)
	if strings.TrimSpace(rc.PlanMode) == "" {
		rc.PlanMode = s.planMode(ctx)
	}
	return rc
}

// planMode 读取当前规划模式（未注入读取源时回落 standard）。
func (s *SOPService) planMode(ctx context.Context) string {
	if s == nil || s.planModeProvider == nil {
		return sop.PlanModeStandard
	}
	return s.planModeProvider(ctx)
}

// disabledIDs 读取用户关闭的规则 ID（失败即视为「全开」，绝不因读取失败而静默关规则）。
func (s *SOPService) disabledIDs(ctx context.Context) []string {
	if s == nil || s.disabledProvider == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return s.disabledProvider(ctx)
}

// Catalog 返回原生 SOP 数据字典（前端设置界面渲染逐条开关的唯一真源）。
func (s *SOPService) Catalog() []sop.Item {
	return sop.Catalog()
}

// SaveCore 保存自定义核心 SOP（首次保存即完成「内置 → 自定义」的 fork）。
//
// 超预算返回包装了 sop.ErrBudgetExceeded 的错误，且不修改磁盘（拒绝不截断）。
func (s *SOPService) SaveCore(_ context.Context, content string) (sop.Core, error) {
	if s == nil || s.loader == nil {
		return sop.Core{}, ErrSOPUnavailable
	}
	return s.loader.SaveCore(content)
}

// ResetCore 删除自定义覆盖层，回落出厂内置。
func (s *SOPService) ResetCore(_ context.Context) (sop.Core, error) {
	if s == nil || s.loader == nil {
		return sop.Core{}, ErrSOPUnavailable
	}
	if err := s.loader.ResetCore(); err != nil {
		return sop.Core{}, err
	}
	return s.loader.LoadCore(), nil
}

// EffectiveSOP 生效预览视图：本轮真正注入模型的 SOP 组合。
//
// 这是「调教 SOP 的反馈回路」——不跑对话就能看清系统提示里到底装了什么。
type EffectiveSOP struct {
	// Core 核心 SOP（SystemBase 人格段）。
	Core sop.Core `json:"core"`
	// Rules 本次上下文生效的工程规则行（含序号，与真实注入逐字一致）。
	Rules []string `json:"rules"`
	// ActiveIDs 生效规则 ID（用于对比目录全集，排查「为什么这条没出现」）。
	ActiveIDs []string `json:"active_ids"`
	// Entries 项目级条目（「## 项目规范（SOP）」段，仅绑定工作区时非空）。
	Entries []sop.Entry `json:"entries"`
	// Workspace 当前会话绑定的工作区路径（空 = 未绑定）。
	Workspace string `json:"workspace"`
	// TotalTokens 核心 + 规则 + 全部条目的 token 合计。
	TotalTokens int `json:"total_tokens"`
	// SystemPreview 最终系统提示的前缀预览（基础提示 + 规则段 + 条目段）。
	SystemPreview string `json:"system_preview"`
	// Truncated 是否有条目因条数上限被截断（D10：必须可见，不得静默）。
	Truncated bool `json:"truncated"`
	// EntryLimit 项目级条目条数上限。
	EntryLimit int `json:"entry_limit"`
}

// Effective 返回生效预览。
//
// rc 为会话事实（IsSub / PlanMode / SessionID）；systemBase 传入当前会话的基础系统提示
// （已含工作区事实），用于拼出真实预览。wsPath 为会话绑定的工作区（空串 = 未绑定）。
func (s *SOPService) Effective(ctx context.Context, systemBase, wsPath string, rc sop.Context) EffectiveSOP {
	core := s.Core(ctx)
	// 先补齐会话外部事实，再让 Rules 与 ActiveIDs 消费同一份上下文
	// （两处各自补齐过一次 → 曾出现「已关闭却仍列为生效」的假象）。
	rc = s.normalize(ctx, rc)
	rules := s.Rules(ctx, rc)
	entries := sop.LoadEntries(wsPath)

	view := EffectiveSOP{
		Core:       core,
		Rules:      rules,
		ActiveIDs:  sop.ActiveIDs(rc),
		Entries:    entries,
		Workspace:  strings.TrimSpace(wsPath),
		EntryLimit: sop.MaxEntries,
	}
	if len(entries) > sop.MaxEntries {
		view.Truncated = true
		entries = entries[:sop.MaxEntries]
		view.Entries = entries
	}
	view.TotalTokens = core.Tokens
	for _, r := range rules {
		view.TotalTokens += sop.EstimateTokens(r)
	}
	for _, e := range entries {
		view.TotalTokens += e.Tokens
	}

	var b strings.Builder
	if base := strings.TrimSpace(systemBase); base != "" {
		b.WriteString(base)
	} else {
		b.WriteString(core.Content)
	}
	if len(rules) > 0 {
		b.WriteString("\n\n## 工作规则（按序遵守，编号仅用于引用）\n")
		b.WriteString(strings.Join(rules, "\n"))
	}
	if len(entries) > 0 {
		b.WriteString("\n\n## 项目规范（SOP）\n\n")
		for _, e := range entries {
			b.WriteString(e.Body)
			b.WriteString("\n\n")
		}
	}
	view.SystemPreview = strings.TrimRight(b.String(), "\n") + "\n"
	return view
}

// BindCore 返回供 RunService 使用的动态核心 SOP 源（每 Run 调用一次）。
//
// 保留本函数以兼容既有装配（core 段单独注入）；工程规则段走 RulesFunc。
// 与既有 setup 的 SOPFunc / ToolRefs 完全同款：不缓存 → 改完下一轮即生效。
func (s *SOPService) BindCore() func() string {
	return s.CoreText
}

// BindRules 返回供 RunService / ChildRunner 使用的动态规则源（每 Run 调用一次）。
func (s *SOPService) BindRules() func(ctx context.Context, rc sop.Context) []string {
	return func(ctx context.Context, rc sop.Context) []string { return s.Rules(ctx, rc) }
}
