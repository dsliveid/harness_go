package retrieval

import (
	"strings"
)

// SelectOptions 动态工具选择的参数（Token 治理 D-2 / D11 三档）。
type SelectOptions struct {
	// Query 本轮用户输入（检索查询串）。
	Query string
	// TopK 动态注入条数上限（推荐 4~8，D11 规定）。<=0 时用 DefaultTopK。
	TopK int
	// MinTopK 降级时允许的最小条数（预算闸门第二级「topK 减半」用它兜底）。
	MinTopK int
	// AlwaysNames 常驻集（core 档 + 最近使用），检索为空时作为回退候选。
	AlwaysNames []string
	// RecentNames 最近使用过的工具名（检索为空时的回退优先级高于纯核心集）。
	RecentNames []string
	// ExcludeNames 已进常驻集、不需要重复注入的工具名。
	ExcludeNames []string
}

// DefaultTopK 动态检索默认条数（D11 推荐 4~8 的中位）。
const DefaultTopK = 6

// DefaultMinTopK 降级后的最小条数。
const DefaultMinTopK = 2

// normalize 归一化参数。
func (o SelectOptions) normalize() SelectOptions {
	out := o
	out.Query = strings.TrimSpace(o.Query)
	if out.TopK <= 0 {
		out.TopK = DefaultTopK
	}
	if out.MinTopK <= 0 {
		out.MinTopK = DefaultMinTopK
	}
	if out.MinTopK > out.TopK {
		out.MinTopK = out.TopK
	}
	return out
}

// SelectedTool 动态选中的工具（含来源标记，便于可观测与调试）。
type SelectedTool struct {
	Key    string
	Score  float64
	Source string // "retrieved" | "recent" | "core-fallback"
}

// SelectTools 按「检索 → 回退」两级策略选出本轮要注入的 deferred 工具。
//
// 铁律（D11「宽进严出」+ D-2 需求）：
//  1. 检索只决定「注入哪些定义」，**不决定能否执行** —— 未注入但已注册的工具仍可调用，
//     故本函数只做筛选，绝不返回「禁止调用」语义。
//  2. 检索为空**必须回退**「核心集 + 最近使用」，否则模型完全看不到按需工具，
//     等于把 deferred 档变成不可发现（比全量注入更糟）。
//  3. TopK 严格上限，防检索把索引条数撑爆成变相全量注入。
//
// 排序优先级：retrieved（按 RRF 分降序）> recent > core-fallback。
// 同一工具只出现一次（先到先得）。
func SelectTools(hits []FusedHit, opts SelectOptions) []SelectedTool {
	o := opts.normalize()

	exclude := make(map[string]struct{}, len(o.ExcludeNames))
	for _, n := range o.ExcludeNames {
		if n = strings.TrimSpace(n); n != "" {
			exclude[n] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, o.TopK)

	out := make([]SelectedTool, 0, o.TopK)
	add := func(key string, score float64, source string) bool {
		if len(out) >= o.TopK {
			return false
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return true
		}
		if _, bad := exclude[key]; bad {
			return true
		}
		if _, dup := seen[key]; dup {
			return true
		}
		seen[key] = struct{}{}
		out = append(out, SelectedTool{Key: key, Score: score, Source: source})
		return true
	}

	// ① 检索命中优先（hits 已由 RRF 排好序）。
	for _, h := range hits {
		if !add(h.Key, h.Score, "retrieved") {
			break
		}
	}
	if len(out) > 0 {
		return out
	}

	// ② 检索为空 → 回退「最近使用 + 核心集」。
	// 刻意保留回退来源标记：日志与 /tools/injection 能看出本轮是「检索命中」还是「兜底」。
	for _, n := range o.RecentNames {
		if !add(n, 0, "recent") {
			break
		}
	}
	for _, n := range o.AlwaysNames {
		if !add(n, 0, "core-fallback") {
			break
		}
	}
	return out
}

// ClampTopK 按降级档位缩放 TopK（预算闸门第二级「topK 减半」）。
//
// 返回不小于 opts.MinTopK 的整数：减半后若低于下限则取下限，
// 保证降级不会把按需工具直接砍到 0（那会退化成「模型完全看不见」）。
func ClampTopK(opts SelectOptions, halve bool) int {
	o := opts.normalize()
	if !halve {
		return o.TopK
	}
	n := o.TopK / 2
	if n < o.MinTopK {
		n = o.MinTopK
	}
	return n
}

// Names 提取选中工具名（保序），供 prompt 层直接使用。
func Names(tools []SelectedTool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Key)
	}
	return out
}
