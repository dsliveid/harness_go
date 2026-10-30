// Package loader 生成工具注入视图：core 全量 / deferred 名称索引 / on_demand 零注入，
// 并给出注入 token 预算明细（供 /api/v1/tools/injection 与启动日志）。
package loader

import (
	"fmt"
	"strings"

	"harness-go/pkg/toolapi"
)

// 常量：deferred 索引单条约 18 token（D11 基线），用于成本估算下限。
const (
	// IndexCostPerTool 单条 deferred 索引的基线成本（token）。
	IndexCostPerTool = 18
	// MaxIndexDescription 索引条目描述的最大字数（超出截断）。
	MaxIndexDescription = 60
)

// Limits 注入软上限（仅告警）。
type Limits struct {
	CoreSoft     int `json:"core_soft"`
	DeferredSoft int `json:"deferred_soft"`
}

// IndexEntry deferred 索引条目（「名称 + 一行描述」，约 18 token）。
type IndexEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	CostTokens  int    `json:"cost_tokens"`
}

// Injection 本轮注入视图与预算明细。
type Injection struct {
	Core           []toolapi.Spec `json:"core"`
	DeferredIndex  []IndexEntry   `json:"deferred_index"`
	OnDemand       []toolapi.Spec `json:"on_demand"`
	DeferredHidden []string       `json:"deferred_hidden,omitempty"` // 因超软上限未进索引的工具名
	CostCore       int            `json:"cost_core"`
	CostIndex      int            `json:"cost_index"`
	CostTotal      int            `json:"cost_total"`
	Limits         Limits         `json:"limits"`
	Warnings       []string       `json:"warnings,omitempty"`
}

// Build 按 tier 拆分并计算成本（tier 已由 registry 应用覆盖，这里不再改档）。
func Build(specs []toolapi.Spec, limits Limits) Injection {
	if limits.CoreSoft <= 0 {
		limits.CoreSoft = 8
	}
	if limits.DeferredSoft <= 0 {
		limits.DeferredSoft = 60
	}
	inj := Injection{Limits: limits}

	var deferred []toolapi.Spec
	for _, s := range specs {
		n := s.Normalized()
		switch n.Tier {
		case toolapi.TierCore:
			inj.Core = append(inj.Core, n)
			inj.CostCore += n.CostTokens
		case toolapi.TierOnDemand:
			inj.OnDemand = append(inj.OnDemand, n)
		default:
			deferred = append(deferred, n)
		}
	}

	if len(inj.Core) > limits.CoreSoft {
		inj.Warnings = append(inj.Warnings, fmt.Sprintf(
			"core 档 %d 条超过软上限 %d（仅告警，不阻断）：请下调部分工具为 deferred",
			len(inj.Core), limits.CoreSoft))
	}
	if len(deferred) > limits.DeferredSoft {
		inj.Warnings = append(inj.Warnings, fmt.Sprintf(
			"deferred 档 %d 条超过软上限 %d（仅告警），仅前 %d 条进入索引",
			len(deferred), limits.DeferredSoft, limits.DeferredSoft))
		inj.DeferredHidden = specNames(deferred[limits.DeferredSoft:])
		deferred = deferred[:limits.DeferredSoft]
	}

	for _, s := range deferred {
		e := IndexEntry{Name: s.Name, Description: firstLine(s.Description, MaxIndexDescription)}
		e.CostTokens = estimateIndexCost(e)
		inj.DeferredIndex = append(inj.DeferredIndex, e)
		inj.CostIndex += e.CostTokens
	}
	inj.CostTotal = inj.CostCore + inj.CostIndex
	return inj
}

// IndexLines 渲染进 system 的索引行（稳定顺序，便于前缀缓存命中）。
//
// 格式：`- <name> — <one line description>`
func (i Injection) IndexLines() []string {
	out := make([]string, 0, len(i.DeferredIndex))
	for _, e := range i.DeferredIndex {
		if e.Description == "" {
			out = append(out, "- "+e.Name)
			continue
		}
		out = append(out, "- "+e.Name+" — "+e.Description)
	}
	return out
}

// Names 返回某档位的工具名列表（core / deferred / on_demand）。
func (i Injection) Names(tier toolapi.Tier) []string {
	switch tier {
	case toolapi.TierCore:
		return specNames(i.Core)
	case toolapi.TierOnDemand:
		return specNames(i.OnDemand)
	default:
		out := make([]string, 0, len(i.DeferredIndex))
		for _, e := range i.DeferredIndex {
			out = append(out, e.Name)
		}
		return out
	}
}

// estimateIndexCost 索引条目成本：`- name — desc` 约 18 token，按字符数校正。
func estimateIndexCost(e IndexEntry) int {
	n := len([]rune(e.Name)) + len([]rune(e.Description)) + 4
	cost := n / 2
	if cost < IndexCostPerTool {
		cost = IndexCostPerTool
	}
	return cost
}

// firstLine 取首行并截断到 max 个字符。
func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	r := []rune(s)
	if max > 0 && len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

func specNames(specs []toolapi.Spec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}
