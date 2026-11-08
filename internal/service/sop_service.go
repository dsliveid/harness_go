package service

import (
	"context"
	"errors"
	"strings"

	"harness-go/internal/sop"
)

// SOPService 核心 SOP（agent-core）用例层。
//
// 职责边界：
//   - 本层只做**编排**：把 HTTP 语义映射到 internal/sop.Loader，并组装
//     「生效预览」这类跨信息聚合的视图；
//   - 真源解析全部下沉到 internal/sop（零依赖内核层），本层不碰文件 IO 细节；
//   - 不持有任何缓存：每轮对话都重新读盘，保证「改完下一轮即生效」（热生效）。
//
// 与 RunService 的协作：RunService 通过 CoreFunc（见 NewSOPService.BindCore）
// 每 Run 取一次核心 SOP，与既有 SOPFunc / ToolRefs 同款模式，不新造机制。
type SOPService struct {
	loader *sop.Loader
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
	// Core 核心 SOP（SystemBase 段）。
	Core sop.Core `json:"core"`
	// Entries 项目级条目（「## 项目规范（SOP）」段，仅绑定工作区时非空）。
	Entries []sop.Entry `json:"entries"`
	// Workspace 当前会话绑定的工作区路径（空 = 未绑定）。
	Workspace string `json:"workspace"`
	// TotalTokens 核心 + 全部条目的 token 合计。
	TotalTokens int `json:"total_tokens"`
	// SystemPreview 最终系统提示的前缀预览（基础提示 + SOP 段）。
	SystemPreview string `json:"system_preview"`
	// Truncated 是否有条目因条数上限被截断（D10：必须可见，不得静默）。
	Truncated bool `json:"truncated"`
	// EntryLimit 项目级条目条数上限。
	EntryLimit int `json:"entry_limit"`
}

// Effective 返回生效预览。
//
// systemBase 传入当前会话的基础系统提示（已含工作区事实），用于拼出真实预览；
// wsPath 为会话绑定的工作区（空串 = 未绑定，则项目级条目为空）。
func (s *SOPService) Effective(_ context.Context, systemBase, wsPath string) EffectiveSOP {
	core := s.Core(context.Background())
	entries := sop.LoadEntries(wsPath)

	view := EffectiveSOP{
		Core:       core,
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
	for _, e := range entries {
		view.TotalTokens += e.Tokens
	}

	var b strings.Builder
	b.WriteString(systemBase)
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
// 与既有 setup 的 SOPFunc / ToolRefs 完全同款：不缓存 → 改完下一轮即生效。
func (s *SOPService) BindCore() func() string {
	return s.CoreText
}
