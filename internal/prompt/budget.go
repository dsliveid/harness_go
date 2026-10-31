package prompt

// 预算闸门五级降级（阶段 D-4）。
//
// 设计约束（务必遵守）：
//   - 本文件只放**纯函数与数据结构**，不依赖 store / retrieval / llm 内核状态，
//     保证降级算法可独立单测；编排（何时调用、用尽后如何报错）在 service 层。
//   - **严禁静默截断**：每一档降级都记录实际动作（Applied + Detail），
//     五级用尽仍超预算时置 Exhausted=true，由上层返回明确错误。
//
// 降级顺序（不可调换，越靠前越优先牺牲「注入深度」而非「已有内容」）：
//  ①省略 L2/L3 记忆 → ②topK 减半 → ③常驻工具降到 4 → ④旧 tool_result 转引用
//  → ⑤强制压缩（外部完成） → 仍超则明确报错

import "fmt"

// DegradeLevel 降级档位（0 = 未降级，按序递进）。
type DegradeLevel int

const (
	// LevelNone 未降级（预算内）。
	LevelNone DegradeLevel = 0
	// LevelDropMemory ①省略 L2/L3 记忆（记忆层未落地时为占位档，直接跳过）。
	LevelDropMemory DegradeLevel = 1
	// LevelHalveTopK ②动态检索 topK 减半（下限 MinTopK）。
	LevelHalveTopK DegradeLevel = 2
	// LevelTrimCoreTools ③常驻工具降到 CoreToolsFloor 条。
	LevelTrimCoreTools DegradeLevel = 3
	// LevelCollapseToolResults ④旧 tool_result 转「工具名 + 摘要 + path:line」引用。
	LevelCollapseToolResults DegradeLevel = 4
	// LevelForceCompress ⑤强制压缩（需外部便宜模型，本层只产出信号）。
	LevelForceCompress DegradeLevel = 5

	// levelMax 最高档位。
	levelMax = LevelForceCompress
)

// CoreToolsFloor 第三级降级后常驻工具保留条数（与计划 §5 一致）。
const CoreToolsFloor = 4

// LevelName 返回档位名称（日志与错误信息用）。
func LevelName(l DegradeLevel) string {
	switch l {
	case LevelNone:
		return "未降级"
	case LevelDropMemory:
		return "省略记忆层"
	case LevelHalveTopK:
		return "topK 减半"
	case LevelTrimCoreTools:
		return "常驻工具降到 4"
	case LevelCollapseToolResults:
		return "旧工具结果转引用"
	case LevelForceCompress:
		return "强制压缩"
	default:
		return fmt.Sprintf("未知档位(%d)", int(l))
	}
}

// DegradeAction 某一档在本次规划中的采用情况。
type DegradeAction struct {
	Level   DegradeLevel `json:"level"`
	Name    string       `json:"name"`
	Applied bool         `json:"applied"`
	// Detail 本档具体做了什么（如「topK 8 → 4」），便于可观测与排障。
	Detail string `json:"detail,omitempty"`
}

// BudgetPlan 降级规划结果（由 service 编排层消费）。
type BudgetPlan struct {
	// Level 本次规划采用到的最高档位。
	Level DegradeLevel `json:"level"`
	// Actions 逐档记录（含未采用的档，便于排查「为什么没降」）。
	Actions []DegradeAction `json:"actions"`
	// TopK 规划后的动态检索条数（仅当第②级被采用时有意义，否则为 0）。
	TopK int `json:"top_k,omitempty"`
	// CoreLimit 规划后的常驻工具上限（仅当第③级被采用时非 0）。
	CoreLimit int `json:"core_limit,omitempty"`
	// DropMemory 是否省略记忆层（第①级）。
	DropMemory bool `json:"drop_memory"`
	// CollapseToolResults 是否把旧工具结果转引用（第④级）。
	CollapseToolResults bool `json:"collapse_tool_results"`
	// NeedCompress 是否需要外部强制压缩（第⑤级）。
	NeedCompress bool `json:"need_compress"`
	// Exhausted 五档全部采用后仍超预算 —— 上层必须返回明确错误，禁止静默放行。
	Exhausted bool `json:"exhausted"`
}

// BudgetParams 降级规划的输入。
type BudgetParams struct {
	// MaxTotal 总预算（<=0 表示不限，直接返回未降级）。
	MaxTotal int
	// Current 当前估算总 token。
	Current int
	// TopK 当前动态注入条数（第②级输入）；<=0 表示本场景不涉及工具检索。
	TopK int
	// MinTopK topK 减半的下限（不得为 0，配置层已硬校验）。
	MinTopK int
	// CoreCount 当前常驻工具条数（第③级输入）。
	CoreCount int
	// HasMemory 是否存在可省略的 L2/L3 记忆段（第①级）。
	HasMemory bool
	// HasOldToolResults 是否存在旧工具结果可转引用（第④级）。
	HasOldToolResults bool
	// AllowCompress 是否允许请求外部强制压缩（第⑤级）。
	AllowCompress bool
}

// PlanDegrade 一次规划：从当前档位**继续往下**找第一个可用的降级动作。
//
// 语义（这是与「一次性算完五档」的关键区别）：
// 每档实际能省多少 token 只能把方案**应用后重新计数**才知道，因此本函数
// 每次只返回「下一个可执行的动作」，由 service 循环调用：
//
//	plan := PlanDegrade(p)          // p.Current 为上一次应用后的真实计数
//	if plan.Exhausted -> 报错
//	应用 plan 的动作 -> 重新 Build -> 更新 p.Current -> 再调 PlanDegrade
//
// from 表示已尝试过的档位，下次从 from+1 起继续，避免重复采用同一档。
func PlanDegrade(p BudgetParams, from DegradeLevel) BudgetPlan {
	plan := BudgetPlan{Level: from, Actions: make([]DegradeAction, 0, 5)}

	if p.MaxTotal <= 0 || p.Current <= p.MaxTotal {
		plan.Level = LevelNone
		return plan // 不限预算或已达标，无需降级
	}

	for lv := from + 1; lv <= levelMax; lv++ {
		act := DegradeAction{Level: lv, Name: LevelName(lv)}
		switch lv {
		case LevelDropMemory:
			if p.HasMemory {
				act.Applied = true
				act.Detail = "省略 L2/L3 记忆段"
				plan.DropMemory = true
			}
		case LevelHalveTopK:
			if n := halve(p.TopK, p.MinTopK); n < p.TopK {
				act.Applied = true
				act.Detail = fmt.Sprintf("topK %d → %d", p.TopK, n)
				plan.TopK = n
			}
		case LevelTrimCoreTools:
			if p.CoreCount > CoreToolsFloor {
				act.Applied = true
				act.Detail = fmt.Sprintf("常驻工具 %d → %d", p.CoreCount, CoreToolsFloor)
				plan.CoreLimit = CoreToolsFloor
			}
		case LevelCollapseToolResults:
			if p.HasOldToolResults {
				act.Applied = true
				act.Detail = "旧工具结果转「工具名 + 摘要 + path:line」引用"
				plan.CollapseToolResults = true
			}
		case LevelForceCompress:
			if p.AllowCompress {
				act.Applied = true
				act.Detail = "请求外部模型强制压缩"
				plan.NeedCompress = true
			}
		}
		plan.Actions = append(plan.Actions, act)
		if act.Applied {
			plan.Level = lv
			return plan // 只返回第一个可用动作，应用后重新计数再续
		}
	}

	// 从 from+1 到最高档都没有可执行动作 ⇒ 已用尽，上层必须明确报错。
	plan.Level = levelMax
	plan.Exhausted = true
	return plan
}

// halve 计算减半后的条数，不低于 min（min<=0 时按 1 兜底）。
func halve(n, min int) int {
	if n <= 0 {
		return 0
	}
	h := n / 2
	floor := min
	if floor < 1 {
		floor = 1
	}
	if h < floor {
		h = floor
	}
	return h
}
