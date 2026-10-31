package service

import (
	"fmt"
	"strings"

	"harness-go/internal/agent"
	"harness-go/internal/llm"
	"harness-go/internal/prompt"
)

// BudgetGate 预算闸门（阶段 D-4）：包装真实上下文组装器，把五级降级串成链路。
//
// 为什么放 service：降级第②级需要理解工具注入语义、第④级要改写历史消息，
// 而 prompt 层禁止依赖检索/工具语义（内核不得反向依赖适配器）——
// service 是唯一同时可见 prompt 与工具/记忆语义的编排点（缺口清单 §2 缺口 1 落位方案）。
//
// 编排模型（与 prompt.PlanDegrade 的契约严格一致）：
//
//	Build 一次 → 超预算？→ PlanDegrade 取「下一个可执行动作」→ 应用到 Input 副本
//	→ 重新 Build 重新计数 → 再规划下一档 → 直到预算内或五档用尽。
//	每档实际省多少只有应用后重新计数才知道，一次性算完五档是错的。
//
// 铁律（严禁静默截断）：
//   - 每一档的应用动作记录在返回结果的 Degrade 里（引擎发事件、日志可见）；
//   - 五档用尽仍超预算 → 返回 OverBudget=true 的结果，由引擎以
//     budget_exhausted 明确失败该 Run，绝不把超限内容静默塞给模型。
type BudgetGate struct {
	inner agent.ContextBuilder
	// MaxTotal 总预算（<=0 = 不限，闸门直通零开销）。
	MaxTotal int
	// MinTopK 第②级 topK 减半的下限（来自 tools.dynamic_min_top_k，配置层已硬校验 >0）。
	MinTopK int
	// AllowCompress 第⑤级是否可用（需外部压缩器；本阶段未接 LLM 压缩，恒 false ——
	// 配置了压缩开关而没有压缩器会变成「假降级」，违反禁静默兜底铁律，故不做配置项）。
	AllowCompress bool
}

// NewBudgetGate 创建预算闸门。inner 为真实组装器（*prompt.Builder 或测试桩）。
func NewBudgetGate(inner agent.ContextBuilder, maxTotal, minTopK int) *BudgetGate {
	return &BudgetGate{inner: inner, MaxTotal: maxTotal, MinTopK: minTopK}
}

// 折叠参数（第④级）。
const (
	// collapseKeepRecent 保留最近 N 条 tool 结果不折叠（当前轮的上下文连续性优先）。
	collapseKeepRecent = 4
	// collapseMinChars 短结果不值得折叠（折叠省不了几个 token 反而丢信息）。
	collapseMinChars = 200
)

// Build 实现 agent.ContextBuilder：预算内直通，超预算按五级降级。
func (g *BudgetGate) Build(in prompt.Input) prompt.BuildResult {
	res := g.inner.Build(in)
	if g.MaxTotal <= 0 || res.Breakdown.Total <= g.MaxTotal {
		return res
	}

	// worked 是 Input 的副本：降级只改本轮注入形态，绝不污染调用方数据
	// （引擎的 history 切片在多个 step 间复用，原地改写会跨步泄漏）。
	worked := cloneInput(in)
	var actions []prompt.DegradeAction

	for i := 0; i <= int(prompt.LevelForceCompress); i++ {
		plan := prompt.PlanDegrade(prompt.BudgetParams{
			MaxTotal:          g.MaxTotal,
			Current:           res.Breakdown.Total,
			TopK:              len(worked.ActiveTools),
			MinTopK:           g.MinTopK,
			CoreCount:         countCoreTools(worked.Tools),
			HasMemory:         worked.MemoryTail != "",
			HasOldToolResults: hasCollapsible(worked.History),
			AllowCompress:     g.AllowCompress,
		}, planLevel(actions))
		if plan.Exhausted {
			// 五档用尽仍超预算：返回带标记的结果，引擎据此明确失败（严禁静默放行）。
			res.OverBudget = true
			res.Degrade = actions
			return res
		}
		act := applyDegrade(&worked, plan)
		actions = append(actions, act)
		res = g.inner.Build(worked)
		if res.Breakdown.Total <= g.MaxTotal {
			res.OverBudget = false
			res.Degrade = actions
			return res
		}
	}
	// 理论不可达（每次循环 plan.Level 严格递增），防御性兜底与 Exhausted 同语义。
	res.OverBudget = true
	res.Degrade = actions
	return res
}

// planLevel 返回已应用到的最高档位（下次从它的下一档继续）。
func planLevel(actions []prompt.DegradeAction) prompt.DegradeLevel {
	if len(actions) == 0 {
		return prompt.LevelNone
	}
	return actions[len(actions)-1].Level
}

// applyDegrade 把规划出的单个动作落到 Input 副本上，返回带 Detail 的动作记录。
func applyDegrade(in *prompt.Input, plan prompt.BudgetPlan) prompt.DegradeAction {
	act := prompt.DegradeAction{Level: plan.Level, Name: prompt.LevelName(plan.Level), Applied: true}
	switch plan.Level {
	case prompt.LevelDropMemory:
		in.MemoryTail = "" // 只省 L2/L3 动态记忆；L0 核心约束不省（行为边界）
		act.Detail = plan.Actions[len(plan.Actions)-1].Detail
	case prompt.LevelHalveTopK:
		// 提升名单截到减半后的条数：注入深度下降，被截掉的 deferred 工具仍可执行。
		n := plan.TopK
		if n > len(in.ActiveTools) {
			n = len(in.ActiveTools)
		}
		in.ActiveTools = in.ActiveTools[:n]
		act.Detail = plan.Actions[len(plan.Actions)-1].Detail
	case prompt.LevelTrimCoreTools:
		in.Tools = trimCoreTools(in.Tools, prompt.CoreToolsFloor)
		act.Detail = plan.Actions[len(plan.Actions)-1].Detail
	case prompt.LevelCollapseToolResults:
		n := collapseToolResults(in.History, collapseKeepRecent, collapseMinChars)
		act.Detail = fmt.Sprintf("折叠 %d 条旧工具结果为引用", n)
	case prompt.LevelForceCompress:
		act.Detail = plan.Actions[len(plan.Actions)-1].Detail
		// 本层只产出信号：外部压缩由上层（未来接 LLM 摘要）完成，
		// 当前允许压缩时本轮重建结果不变，由调用方继续循环判定。
	}
	return act
}

// cloneInput 复制 Input（浅拷贝切片头，降级会替换/截断的切片在此复制）。
func cloneInput(in prompt.Input) prompt.Input {
	out := in
	out.ActiveTools = append([]string(nil), in.ActiveTools...)
	out.Tools = append([]prompt.ToolRef(nil), in.Tools...)
	out.History = append([]llm.Message(nil), in.History...)
	return out
}

// countCoreTools 统计 core 档工具条数（第③级输入）。
func countCoreTools(tools []prompt.ToolRef) int {
	n := 0
	for _, t := range tools {
		if t.Tier == prompt.TierCore {
			n++
		}
	}
	return n
}

// trimCoreTools 把 core 档裁到 keep 条（保持注册顺序 = 推荐顺序），其余档位不动。
// 铁律（宽进严出）：被裁掉的 core 工具仍可执行（registry 不变），只是本轮不注入定义。
func trimCoreTools(tools []prompt.ToolRef, keep int) []prompt.ToolRef {
	if keep <= 0 {
		return tools
	}
	out := make([]prompt.ToolRef, 0, len(tools))
	kept := 0
	for _, t := range tools {
		if t.Tier == prompt.TierCore {
			if kept >= keep {
				continue
			}
			kept++
		}
		out = append(out, t)
	}
	return out
}

// hasCollapsible 判断历史中是否存在可折叠的旧工具结果（第④级输入）。
func hasCollapsible(history []llm.Message) bool {
	return countCollapsible(history, collapseKeepRecent, collapseMinChars) > 0
}

// collapseBoundary 返回「旧 tool 消息」的上界下标：最近 keep 条 tool 消息中
// 最靠前的那条的下标；不足 keep 条返回 -1（无可折叠对象）。
func collapseBoundary(history []llm.Message, keep int) int {
	seen := 0
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != llm.RoleTool {
			continue
		}
		seen++
		if seen == keep {
			return i
		}
	}
	return -1
}

// collapseToolResults 把「最近 keep 条之外、正文超过 minChars 的 tool 消息」替换为
// 「工具名 + 已折叠引用」。短结果与最近结果原样保留。
// 返回折叠条数。只改副本（cloneInput 已复制），不影响引擎的历史切片。
func collapseToolResults(history []llm.Message, keep, minChars int) int {
	boundary := collapseBoundary(history, keep)
	if boundary <= 0 {
		return 0
	}
	// tool 消息没有工具名（在前置 assistant 的 ToolCalls 里），先建 callID → name 映射。
	names := make(map[string]string)
	for _, m := range history {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && tc.Name != "" {
				names[tc.ID] = tc.Name
			}
		}
	}
	n := 0
	for i := 0; i < boundary; i++ {
		m := &history[i]
		if m.Role != llm.RoleTool || len([]rune(m.Content)) < minChars {
			continue
		}
		name := names[m.ToolCallID]
		if name == "" {
			name = "unknown"
		}
		m.Content = fmt.Sprintf("[tool:%s 结果已折叠（预算降级）：原 %d 字符，需要时请重新调用该工具]",
			name, len([]rune(m.Content)))
		n++
	}
	return n
}

// countCollapsible 与 collapseToolResults 同口径的只读计数。
func countCollapsible(history []llm.Message, keep, minChars int) int {
	boundary := collapseBoundary(history, keep)
	if boundary <= 0 {
		return 0
	}
	n := 0
	for i := 0; i < boundary; i++ {
		if history[i].Role == llm.RoleTool && len([]rune(history[i].Content)) >= minChars {
			n++
		}
	}
	return n
}

// DegradeSummary 把降级动作串成一行日志/事件文本。
func DegradeSummary(actions []prompt.DegradeAction) string {
	if len(actions) == 0 {
		return ""
	}
	parts := make([]string, 0, len(actions))
	for _, a := range actions {
		parts = append(parts, a.Name+"："+a.Detail)
	}
	return strings.Join(parts, "；")
}
