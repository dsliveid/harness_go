// Package sop —— 原生 SOP 目录（结构化定义层）。
//
// # 为什么存在这个文件
//
// 改造前 Go 版的「SOP」只有 internal/sop/builtin/agent-core.md 里的三条口语化规则
// （≈90 token），而老版本 harness_mini 早已把工程规范做成**结构化目录**
// （src-tauri/src/sop_catalog.rs）：每条带 层级 / 分类 / 作用域（主·子·通用）/
// 多变体（Always Plan / Always Proceed / 标准 / 主进程 / 子进程）/ 逐条启停开关。
// 结果是：模型在 Go 版里看不到「先声明意图再改代码」「按职责拆分子 Agent」
// 「漏斗式研读」「边读边记」等真正的作业程序，只能靠用户每次手写。
//
// 本文件即「补全后的原生 SOP 字典」，逐条对齐老版本口径（文案保留原有锚点，
// 便于两端对比与迁移），并补两条 Go 内核实际需要的规范（路径基准、交付口径）。
//
// # 与 agent-core.md 的关系（单一真源，不双写）
//
//	agent-core.md  人格 + 安全底线（可被 data/sop/agent-core.md 覆盖、受 token 预算约束）
//	catalog.go     工程作业程序（**代码即真源**，不可被用户正文覆盖，只可逐条启停）
//
// 两者由 service.SOPService 组合成最终 SystemBase：人格段在前（稳定前缀），
// 规则清单在后。任何规则文案改动只改本文件，绝不散落到 service / api。
//
// # 铁律
//
//   - 本包只依赖标准库（**严禁** import toolapi / plugins / service），
//     内核与插件都可安全向下依赖，依赖方向永远单向。
//   - 作用域硬过滤：main_only 绝不出现在子 Run，sub_only 绝不出现在主 Run。
//   - 硬规则（is_hard_rule）不可关闭 —— 关掉会直接放开安全红线。
package sop

import "strings"

// 作用域取值。
const (
	// ScopeUniversal 主/子会话均生效。
	ScopeUniversal = "universal"
	// ScopeMainOnly 仅主会话生效（如「派生子 Agent」只有主进程能做）。
	ScopeMainOnly = "main_only"
	// ScopeSubOnly 仅子会话生效（如「黑板交付规范」）。
	ScopeSubOnly = "sub_only"
)

// 规划模式取值（与前端「项目设置 → 规划模式」一致）。
const (
	// PlanModeStandard 标准模式：不做强计划门禁。
	PlanModeStandard = "standard"
	// PlanModeAlwaysPlan 强计划确认：生成计划后必须停下等用户确认。
	PlanModeAlwaysPlan = "always_plan"
	// PlanModeAlwaysProceed 自动推进：生成计划后直接施工。
	PlanModeAlwaysProceed = "always_proceed"
)

// Meta 一条 SOP 的元数据（声明用，不含 name —— 界面标题由 Variants[0].Title 派生，
// 保证「界面显示的名字」与「提示词里真实出现的标题」永远同源）。
type Meta struct {
	// ID 稳定标识（前端开关的键；一经发布不得改名，否则用户配置失效）。
	ID string
	// Phase 层级序号（1 前置门禁 / 2 规划执行 / 3 安全红线）。
	Phase int
	// PhaseLabel 层级中文名。
	PhaseLabel string
	// Category 分类键（thinking / workflow / orchestration / quality / memory / safety）。
	Category string
	// CategoryLabel 分类中文名。
	CategoryLabel string
	// Scope 作用域：universal / main_only / sub_only。
	Scope string
	// ScopeLabel 作用域中文名。
	ScopeLabel string
	// IsHardRule 硬规则：常驻生效，前端开关置灰，后端忽略 disabled。
	IsHardRule bool
	// Description 界面说明（这条规范要求什么）。
	Description string
	// DisableEffect 关闭后的后果（界面必须如实告知，杜绝「关了不知道会发生什么」）。
	DisableEffect string
}

// Variant 一条规则的某个具体形态（= 提示词中真实可能出现的一种措辞）。
type Variant struct {
	// Label 「主进程」「子进程」「Always Plan」等，仅用于界面对比。
	Label string
	// Title 提示词里的真标题（渲染为【Title】）。
	Title string
	// Body 正文（【】：之后的全部内容）；{report_file} 为子任务报告文件名占位符。
	Body string
	// Active 该变体在本次上下文中是否生效。
	Active func(Context) bool
}

// Definition 一条 SOP 定义。
type Definition struct {
	Item     Meta
	Variants []Variant
}

// Context 渲染上下文（一次性算好，供所有变体判定使用）。
type Context struct {
	// SessionID 会话 ID（用于替换 {report_file} 占位符）。
	SessionID string
	// IsSub 是否为子会话（子 Agent / 子进程 / 协作者 / 任意 parent 存在者）。
	IsSub bool
	// IsCollaborator 是否为常驻协作者（子会话的子集，部分规范对协作者豁免）。
	IsCollaborator bool
	// PlanMode 规划模式（standard / always_plan / always_proceed）。
	PlanMode string
	// Disabled 被用户关闭的规则 ID（硬规则忽略本字段）。
	Disabled []string
}

// normalizePlanMode 空值与未知值统一回落 standard（保守：不强加计划门禁）。
func normalizePlanMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case PlanModeAlwaysPlan:
		return PlanModeAlwaysPlan
	case PlanModeAlwaysProceed:
		return PlanModeAlwaysProceed
	default:
		return PlanModeStandard
	}
}

// isDisabled 判断规则是否被用户关闭（硬规则恒为 false）。
func (c Context) isDisabled(def Definition) bool {
	if def.Item.IsHardRule {
		return false
	}
	for _, id := range c.Disabled {
		if id == def.Item.ID {
			return true
		}
	}
	return false
}

// lines 把多行正文拼成一段（避免反引号与 Go 原始字符串冲突）。
func lines(ls ...string) string { return strings.Join(ls, "\n") }

// subOnly 判定：子会话（含协作者）。
func subOnly(c Context) bool { return c.IsSub }

// Definitions 返回全部 SOP 定义（顺序即提示词中的出现顺序）。
//
// 顺序设计：门禁 → 规划 → 执行 → 质量 → 知识 → 安全。同一层级内按「先想后做」排列，
// 便于模型自上而下遵行。
func Definitions() []Definition {
	return []Definition{
		// ================= 层级 1：前置意图与写入门禁 =================
		{
			Item: Meta{
				ID:            "path_baseline",
				Phase:         1,
				PhaseLabel:    "第一层级 · 前置意图与写入门禁",
				Category:      "workflow",
				CategoryLabel: "任务规划",
				Scope:         ScopeUniversal,
				ScopeLabel:    "主/子对话可用",
				IsHardRule:    false,
				Description:   "所有相对路径默认相对于工作区根目录解析；子会话与主会话的工作区根严格一致，绝不自造基准目录。",
				DisableEffect: "禁用后：模型需每次从工具返回结果自行推断路径基准，跨会话路径幻觉风险上升。",
			},
			Variants: []Variant{
				{
					Label: "主进程",
					Title: "路径基准与工作区锚定",
					Body: lines(
						"所有相对路径默认相对于工作区根目录解析（见上方「## 工作区」段），当前工作区为主要开发上下文。",
						"若会话未绑定工作区，工具的相对路径落在服务端进程目录，**不得**声称自己运行在用户的某个项目内。",
					),
					Active: func(c Context) bool { return !c.IsSub },
				},
				{
					Label: "子进程",
					Title: "路径基准与工作区锚定",
					Body: lines(
						"你的合法工作区根目录与主会话严格保持一致，所有工具均以该根目录为基准。",
						"严禁自造基准目录、严禁越界读写工作区之外的文件。",
					),
					Active: subOnly,
				},
			},
		},

		{
			Item: Meta{
				ID:            "todo_first_gate",
				Phase:         1,
				PhaseLabel:    "第一层级 · 前置意图与写入门禁",
				Category:      "thinking",
				CategoryLabel: "思考范式",
				Scope:         ScopeUniversal,
				ScopeLabel:    "主/子对话可用",
				IsHardRule:    false,
				Description:   "动代码前必须先声明施工意图（小型任务调用 todo，复杂工程调用 create_plan，二选一即可）；只读勘察严格限制在 ≤8 步以内，查清直接答复；若达 8 步仍无法梳理意图必须立即停下向用户/父进程汇报事实并对齐阻碍；落盘计划与子进程施工统一遵守此项门禁。",
				DisableEffect: "禁用后：Agent 免除动代码前的强制 todo 立项约束，且不触发 8 步勘察上限熔断，可自由读写探索。",
			},
			Variants: []Variant{
				{
					Label: "主进程",
					Title: "Todo 先行与阶梯门禁公理（所有交互必须严格遵守）",
					Body: lines(
						"- 门禁 ①【Todo 先行与写入前置拦截】：",
						"* 严禁在未声明意图（无 active todo 且无 active plan）的情况下擅自调用 `write_file` 或 `edit_file` 修改项目文件；",
						"* 只要涉及修改代码/文件，动工前必须先声明施工意图（小型微调调用 `todo` 工具声明任务列表，复杂架构工程调用 `create_plan` 工具生成实施方案，二选一即可，严禁重复立项）；",
						"* 纯咨询、诊断排查或问答任务同样允许调用 `todo` 工具梳理勘察要点与任务清单，但未经用户明确要求修改，严禁擅自修改文件。",
						"- 门禁 ②【≤8 步勘察上限与目标对齐（防盲目漫游）】：",
						"* 勘察排查类只读工具（glob / list_dir / read_file / grep / file_outline）调用累计不大于 8 步；",
						"* 若在 8 步内已查清事实且无需修改代码，立即在正文中完整回复用户并结案；",
						"* 若在 8 步内明确需要修改代码，必须先声明意图（小型调整/探索调用 `todo`，复杂跨模块工程调用 `create_plan`，二选一即可，严禁重复创建），方可进行代码修改并按清单推进；",
						"* 🚨 熔断停下：若工具调用达到第 8 步仍未能梳理出清晰的意图声明（todo 列表或 plan 方案），严禁凭空脑补计划、严禁继续只读漫游，必须立即停止调用工具，向用户汇报当前事实并对齐目标！",
						"- 门禁 ③【已有计划豁免】：",
						"* 若当前会话已存在用户明确确认的落盘方案（active_plan_id 有效），因其已持有明确的任务蓝图，天然免除重复创建 todo 的要求，严格遵从既定蓝图推进。",
					),
					Active: func(c Context) bool { return !c.IsSub },
				},
				{
					Label: "子进程",
					Title: "Todo 先行与阶梯门禁公理（所有交互必须严格遵守）",
					Body: lines(
						"- 门禁 ①【Todo 先行与写入前置拦截】：",
						"* 严禁在未声明意图（无 active todo 且无 active plan）的情况下擅自调用 `write_file` 或 `edit_file` 修改项目代码；",
						"* 只要涉及修改代码，动工前必须先声明意图（小型任务调用 `todo` 工具声明步骤清单，复杂架构任务调用 `create_plan` 工具生成方案，二选一即可）；",
						"* 纯探索调研、排查诊断或问答任务：未经明确要求修改代码，严禁调用 todo，更严禁擅自修改文件；写入交付报告（.harness/subtasks/{report_file}）天然豁免 todo 门禁。",
						"- 门禁 ②【≤8 步勘察上限与目标对齐（防盲目漫游）】：",
						"* 勘察排查类只读工具（glob / list_dir / read_file / grep / file_outline）调用累计不大于 8 步；",
						"* 若在 8 步内已查清事实且无需修改代码，立即调用 `write_file` 写入交付报告（.harness/subtasks/{report_file}）并结案；",
						"* 若在 8 步内明确需要修改代码，必须先声明意图（小型任务调用 `todo`，复杂架构任务调用 `create_plan`，二选一即可，严禁重复创建），再开始编写代码并按清单推进；",
						"* 🚨 熔断停下：若工具调用达到第 8 步仍未能梳理出清晰的意图声明，或发现前置条件严重缺失，严禁凭空脑补计划、严禁继续只读漫游，必须立即调用 `write_file` 写入交付报告向主进程汇报具体阻碍并交回决策权！",
					),
					Active: subOnly,
				},
			},
		},

		// ================= 层级 2：工程规划与落地执行 =================
		{
			Item: Meta{
				ID:            "plan_first",
				Phase:         2,
				PhaseLabel:    "第二层级 · 工程规划与落地执行",
				Category:      "thinking",
				CategoryLabel: "思考范式",
				Scope:         ScopeUniversal,
				ScopeLabel:    "主/子对话可用",
				IsHardRule:    false,
				Description:   "面对大改动任务（3 个以上文件）或多轮需求，在 .harness/plans/ 自动建立结构化计划 MD 文档；需求变更时优先更新计划，后续严格根据计划分步执行，防止多轮需求失真与遗忘；若已有落盘计划，天然满足代码写入门禁。",
				DisableEffect: "禁用后：Agent 接到任务后可直接编写代码，不强制在 .harness/plans/ 建立结构化方案文档。",
			},
			Variants: []Variant{
				{
					Label: "Always Plan",
					Title: "严格规划先行与确认门禁准则（Always Plan 模式）",
					Body: lines(
						"- 适用范围：凡是对项目进行较大改动（预计修改 3 个以上文件、架构重构、引入新架构或新增功能特性）：",
						"- 阶段划分（严禁越界）：",
						"1. 【只读调研】：仅允许使用只读工具（glob / grep / list_dir / file_outline / read_file）摸清代码逻辑；",
						"2. 【生成计划】：必须先调用 `create_plan` 工具在 `.harness/plans/` 目录下生成结构化计划 MD 文档（已建方案无需重复创建 todo）；",
						"3. 【必须停止并等待确认】：**生成计划后，当前轮次严禁调用任何写文件或编辑代码的工具（write_file / edit_file）**！你必须在回复中简明扼要汇报方案核心、涉及文件与分步清单，并明确请用户审阅确认。只有在用户确认计划后，下一轮对话才可开始修改代码。",
						"- 微小改动（如修改单文件拼写错误、纯问答解释）无需生成计划，可直接处理。",
					),
					Active: func(c Context) bool {
						return !c.IsSub && normalizePlanMode(c.PlanMode) == PlanModeAlwaysPlan
					},
				},
				{
					Label: "Always Proceed",
					Title: "规划先行与自动推进准则（Always Proceed 模式）",
					Body: lines(
						"- 适用范围：凡是对项目进行较大改动（预计修改 3 个以上文件、跨模块重构、新增功能特性）：",
						"- 核心流程：",
						"1. 【必须先有计划】：调用 `create_plan` 工具生成结构化计划 MD 文档落盘，确立任务锚点与分步 Checklist（已建方案无需重复创建 todo）；",
						"2. 【自动推进与意图判断】：生成计划后：",
						"* 若用户在指令中明确说明了“先不改动代码 / 仅出方案 / 暂勿修改 / 先评估”，则生成计划后停止，严禁改动代码；",
						"* 若用户没有明确说明先不改代码，**生成计划后无需停下来等待用户确认，直接连贯执行计划、调用 edit_file / write_file 编写代码**，按清单逐步推进直至完成并自检！",
						"- 微小改动（如单文件微调、纯解释）可直接处理。",
					),
					Active: func(c Context) bool {
						return normalizePlanMode(c.PlanMode) == PlanModeAlwaysProceed || c.IsSub
					},
				},
				{
					Label: "标准模式",
					Title: "方案先行与任务计划中枢治理规范（防需求失真）",
					Body: lines(
						"- 大改动物理落盘先行：凡是涉及核心业务架构重构、多模块接口改造或预计修改 3 个以上核心文件的复杂任务，直接调用 `create_plan` 生成结构化方案文档；已有方案天然豁免 todo，执行阶段严格以方案 Checklist 为基准推进。",
						"- 需求变更优先更新计划：在多轮对话中，一旦用户提出需求调整或纠偏，改动代码前必须首先调用 `update_plan` 同步更新计划文档，彻底杜绝需求失真。",
						"- 按清单逐步推进与结案：全部任务完成并通过测试后，调用 `update_plan(status: \"completed\")` 结案归档。",
					),
					Active: func(c Context) bool {
						if c.IsSub {
							return false
						}
						return normalizePlanMode(c.PlanMode) != PlanModeAlwaysPlan
					},
				},
			},
		},

		{
			Item: Meta{
				ID:            "todo_lifecycle",
				Phase:         2,
				PhaseLabel:    "第二层级 · 工程规划与落地执行",
				Category:      "workflow",
				CategoryLabel: "任务规划",
				Scope:         ScopeUniversal,
				ScopeLabel:    "主/子对话可用",
				IsHardRule:    false,
				Description:   "针对小的调整和探索，可以调用 todo 工具列出任务列表，并随进展更新各项任务状态；在执行完最后一步、给出最终回复前，务必调用 todo 工具将已完成任务的状态更新为 done，切勿遗留 in_progress 状态。",
				DisableEffect: "禁用后：允许 Agent 自由多步推进，不强制调用 todo 工具维护状态清单。",
			},
			Variants: []Variant{{
				Label: "通用",
				Title: "任务清单全生命周期 SOP",
				Body:  "针对小的调整和探索，可以调用 todo 工具列出任务列表，并随进展更新各项任务状态；在执行完最后一步、给出最终回复前，务必调用 todo 工具将已完成任务的状态更新为 done，切勿遗留 in_progress 状态。",
				Active: func(Context) bool {
					return true
				},
			}},
		},

		{
			Item: Meta{
				ID:            "subagent_orchestration",
				Phase:         2,
				PhaseLabel:    "第二层级 · 工程规划与落地执行",
				Category:      "orchestration",
				CategoryLabel: "协同体系",
				Scope:         ScopeMainOnly,
				ScopeLabel:    "仅主对话",
				IsHardRule:    false,
				Description:   "面对复杂需求、多模块并发开发（如前后端分离、多独立模块并行）时，可按职责派生专职子 Agent 协同推进。",
				DisableEffect: "禁用后：停用多子进程协作编排指引，Agent 倾向于在单主进程内直接就地处理全部任务。",
			},
			Variants: []Variant{{
				Label: "主进程",
				Title: "团队协同委派原则、弹性多并发与职责拆分准则",
				Body: lines(
					"- 面对跨模块、多领域（如前后端并行开发、独立脚本与界面改造等）大体量任务时，可按需调用 `spawn_subagent` 委派专职子 Agent 并行推进。",
					"- 🎯【按职责拆分与工作量控制原则（必须严格遵守）】：",
					"① 【按职责拆分，严禁按步数生硬切分】：子任务拆分必须严格按照系统架构、关注点分离与模块职责进行拆解（例如前端 UI 交互、后端接口逻辑、数据持久化、测试编写等），严禁按“第 1~50 步做什么、第 51~100 步做什么”这种步数阶段来切分任务；",
					"② 【工作量尽量控制在 50 步以内】：主任务在划分每个职责子任务的边界时，应合理评估工作量，将其尽量控制在 50 步工具调用以内能够独立闭环的原子体量；若某一职责体量过大，应进一步按子职责细分，以便派生更多并行子任务；",
					"③ ⚠️【无需主任务提醒子任务步数】：主任务在下发任务描述（task）时，切勿向子任务提及或限制 50 步的范围，子任务的记步器会自动把控；主任务只需专注于讲透业务职责、代码范围（subpath）、接口契约与验收交付标准；",
					"- 🧭【委派前雷达探针与定界规范】：准备委派前，主进程先用 1~2 步只读工具快速识别技术栈与入口文件，完成宏观地貌锚定；交接书需明确初始探查入口（target_anchors）与严格负向禁止项（negative_scope，如严禁全盘通配扫描 dir /s）；",
					"- 🚨【自检验收与打回纪律】：自检验收发现子任务交付的代码有缺陷时，主架构师调用 `reject_subagent` 打回返工，非必要情况禁止私自编码。",
					"- 弹性多子 Agent 并行：面对大型复杂任务，支持同时派发多个前端和多个后端子 Agent 开展高并发开发，必须划分互不正交的战场范围。",
				),
				Active: func(Context) bool {
					return true
				},
			}},
		},

		{
			Item: Meta{
				ID:            "subagent_deliver",
				Phase:         2,
				PhaseLabel:    "第二层级 · 工程规划与落地执行",
				Category:      "orchestration",
				CategoryLabel: "协同体系",
				Scope:         ScopeSubOnly,
				ScopeLabel:    "子对话可用",
				IsHardRule:    false,
				Description:   "专职子任务在完成施工、纯调研结案或遇到重大阻碍时，必须调用 write_file 工具向黑板文件 .harness/subtasks/{id}_report.md 正式交付结构化成果。落盘后子会话立即优雅结案。",
				DisableEffect: "子对话专属交付规范，主对话天然跳过。",
			},
			Variants: []Variant{{
				Label: "子任务",
				Title: "纯黑板交付规范与结案硬性标准",
				Body: lines(
					"- 任务目标、验收标准与负向禁区已在黑板交接书中权威定义；",
					"- 无论任务是全部完成、部分完成或遇到重大阻碍，【唯一硬性结案标准】是调用 `write_file` 工具将结构化交付报告写入 .harness/subtasks/{report_file}：",
					"* 报告内容需详尽记录：执行结论、事实依据、架构/时序图（Mermaid）、涉及改动的文件列表及自检测试输出；",
					"* 若任务受阻，需在报告中明确陈述阻碍原因与前置缺失；",
					"- 写入交付报告后，在对话正文中简短总结交付结论即可自然结束；严禁仅输出文本回复而不落盘报告；",
					"- 【单层执行准则】：严禁再次派生子 Agent，直接使用现有工具高效完成分配的目标。",
				),
				Active: func(Context) bool {
					return true
				},
			}},
		},

		{
			Item: Meta{
				ID:            "subagent_worker_blueprint",
				Phase:         2,
				PhaseLabel:    "第二层级 · 工程规划与落地执行",
				Category:      "orchestration",
				CategoryLabel: "协同体系",
				Scope:         ScopeSubOnly,
				ScopeLabel:    "子对话可用",
				IsHardRule:    false,
				Description:   "子任务作为实施工人，严格遵从主进程蓝图进行精确修改与自检，严禁擅自改动方法签名、接口名或推翻既定全局设计。",
				DisableEffect: "子对话专属准则，主对话天然跳过。",
			},
			Variants: []Variant{{
				Label: "专职子任务",
				Title: "施工工人准则与架构遵从（必须严格遵守）",
				Body: lines(
					"- 若主进程在简报或交接书中提供了《实施蓝图》、拟定函数签名或目标文件与行号锚点，你作为实施工人，【严禁重新设计接口签名、擅自改动方法名或推翻既定架构】；",
					"- 你的核心职责是：使用 read_file 对目标局部精读确认原文，严格按照既定蓝图进行精确修改并运行测试验证；",
					"- 若在执行中发现计划或方案存在重大隐患，请在交付报告 .harness/subtasks/{report_file} 中提出《计划调整建议》，交由主进程统筹处理。",
				),
				Active: func(c Context) bool { return c.IsSub && !c.IsCollaborator },
			}},
		},

		{
			Item: Meta{
				ID:            "surgical_code_reading",
				Phase:         2,
				PhaseLabel:    "第二层级 · 工程规划与落地执行",
				Category:      "quality",
				CategoryLabel: "工程质量",
				Scope:         ScopeUniversal,
				ScopeLabel:    "主/子对话可用",
				IsHardRule:    false,
				Description:   "探索业务与代码时遵循漏斗式渐进探索（目录/清单 → file_outline 骨架 → grep 线索 → 局部靶向精读）；严禁对大型文件进行多轮无休止的分页切片（offset_line）式逐行通读；修改前只精读最小必要上下文，严禁凭空盲猜。",
				DisableEffect: "禁用后：允许 Agent 自由阅读大段源码，解除对连续分页通读与精益读取的强约束。",
			},
			Variants: []Variant{{
				Label: "通用",
				Title: "精益代码研读与规范探索 SOP",
				Body: lines(
					"- 漏斗式探索原则：优先使用 glob / list_dir 查看目录与配置，对源码文件优先调用 `file_outline` 获取结构骨架，结合 `grep` 定位关键线索；",
					"- 项目源码、配置文件阅读优先使用原生工具（read_file、grep、file_outline），深入阅读时以函数/局部为靶标，结合 offset_line 与 max_lines 阅读必要上下文（建议 100~500 行）；修改前必须局部 read_file 确认原文；",
					"- 仅在复杂数据分析或系统底层勘察时允许编写脚本探查，切勿为了读取普通代码文件而舍近求远编写临时脚本。",
				),
				Active: func(Context) bool {
					return true
				},
			}},
		},

		{
			Item: Meta{
				ID:            "safe_code_edit",
				Phase:         2,
				PhaseLabel:    "第二层级 · 工程规划与落地执行",
				Category:      "quality",
				CategoryLabel: "工程质量",
				Scope:         ScopeUniversal,
				ScopeLabel:    "主/子对话可用",
				IsHardRule:    false,
				Description:   "修改文件前必须先用 read_file 读取相关内容，用 edit_file 做基于精确原文的最小化修改；新文件才用 write_file；动手前探索代码结构，修改后尽量用 run_command 运行构建或测试验证。",
				DisableEffect: "禁用后：移除代码最小化精确修改与构建验证的强指引约束。",
			},
			Variants: []Variant{{
				Label: "通用",
				Title: "代码克制与精确修改规范",
				Body: lines(
					"- 修改文件前必须先用 read_file 读取相关内容，用 edit_file 做基于精确原文的最小化修改；新文件才用 write_file；",
					"- 动手前先用 glob / grep / list_dir / file_outline 探索并理解代码结构；",
					"- 修改完成后，尽量用 run_command 运行构建或测试来验证改动。",
				),
				Active: func(Context) bool {
					return true
				},
			}},
		},

		{
			Item: Meta{
				ID:            "memory_distill",
				Phase:         2,
				PhaseLabel:    "第二层级 · 工程规划与落地执行",
				Category:      "memory",
				CategoryLabel: "知识资产",
				Scope:         ScopeUniversal,
				ScopeLabel:    "主/子对话可用",
				IsHardRule:    false,
				Description:   "内置工作区长期认知记忆（.harness/memory/）；常见技术栈提问优先秒级召回；深入探索或阅读多个文件后强制调用 record_memory 沉淀技术大盘与主题碎记；对话完成后台自动萃取 digests。",
				DisableEffect: "禁用后：移除强制调用 record_memory 沉淀约束，且会话结束后不触发后台自动提炼。",
			},
			Variants: []Variant{{
				Label: "通用",
				Title: "边读边记与强制沉淀 SOP",
				Body:  "工作区内置了长期认知记忆系统（存储于 `.harness/memory/`）。下方已自动载入本项目沉淀的技术大盘与工程规范。阅读分析核心代码后，在任务交付前通过 `record_memory` 沉淀高价值事实。",
				Active: func(Context) bool {
					return true
				},
			}},
		},

		// ================= 层级 3：系统交互与安全红线 =================
		{
			Item: Meta{
				ID:            "delivery_etiquette",
				Phase:         3,
				PhaseLabel:    "第三层级 · 系统交互与安全红线",
				Category:      "workflow",
				CategoryLabel: "任务规划",
				Scope:         ScopeUniversal,
				ScopeLabel:    "主/子对话可用",
				IsHardRule:    false,
				Description:   "全程简体中文与用户交流；最终回复简洁总结（做了什么、改了哪些文件、验证结果如何）；思考过程仅用于内部推导，面向用户的答复必须且只能输出在正文（Content）中，严禁思考后停止生成导致正文留空。",
				DisableEffect: "禁用后：解除「结论必须落在正文」的强制口径，深度思考模型可能出现正文留空。",
			},
			Variants: []Variant{{
				Label: "通用",
				Title: "交互语言与交付口径",
				Body: lines(
					"- 全程使用简体中文与用户交流；最终回复简洁总结：做了什么、改了哪些文件、验证结果如何。",
					"- 【思考与交付通道分离】：若当前模型具备深度思考能力（Thinking/Reasoning），思考过程仅用于内部推导与推演；面向用户的答复必须且只能完整输出在正文（Content）中，严禁在思考结束后直接停止生成导致正文留空。",
				),
				Active: func(Context) bool { return true },
			}},
		},

		{
			Item: Meta{
				ID:            "visual_artifact",
				Phase:         3,
				PhaseLabel:    "第三层级 · 系统交互与安全红线",
				Category:      "quality",
				CategoryLabel: "工程质量",
				Scope:         ScopeUniversal,
				ScopeLabel:    "主/子对话可用",
				IsHardRule:    true,
				Description:   "图像与海报必须调用 generate_image 工具；架构/流程图必须直接输出 Markdown Mermaid 代码块，严禁写 Python 脚本绘制静态图。",
				DisableEffect: "系统级质量规范，常驻生效。",
			},
			Variants: []Variant{{
				Label: "通用",
				Title: "双轨视觉感知与生成规范（严禁写 Python 脚本画图/图标）",
				Body: lines(
					"- 图像与图标生成：当用户要求绘制图片、插画、LOGO、应用图标、海报或各类视觉图形时，**必须直接调用 `generate_image` 工具生成**；【严禁编写 Python 脚本（如 PIL/turtle/matplotlib）来绘制图标或生成图片】！生成的图片会自动保存在本地并在界面直接渲染；",
					"- 架构图表与流程图：当用户需要流程图、架构图、序列图、状态机图、类图或数据走向图时，**必须直接输出 Markdown mermaid 围栏代码块或内联 SVG**（界面已原生支持交互式 Mermaid 图表渲染），严禁使用 Python 脚本生成静态图片或在终端打印 ASCII 拼图；",
					"- 图像感知识别：当需要查看、识别或分析本地图片文件时，直接调用 `recognize_image(path, prompt)` 工具，严禁使用 read_file 读取二进制图片。",
				),
				Active: func(Context) bool { return true },
			}},
		},

		{
			Item: Meta{
				ID:            "process_safety_boundary",
				Phase:         3,
				PhaseLabel:    "第三层级 · 系统交互与安全红线",
				Category:      "safety",
				CategoryLabel: "安全底线",
				Scope:         ScopeUniversal,
				ScopeLabel:    "主/子对话可用",
				IsHardRule:    true,
				Description:   "严禁按进程名称全局通配强杀（如 killall/taskkill /IM），进程管理必须精确到具体 PID；严禁执行递归删除、格式化磁盘等破坏性命令。",
				DisableEffect: "核心安全红线，常驻生效，不可关闭。",
			},
			Variants: []Variant{{
				Label: "通用",
				Title: "进程生命周期与破坏性操作安全红线（严防自杀与误杀）",
				Body: lines(
					"- 严禁按进程名称全局通配强杀：绝对禁止执行任何 `Stop-Process -Name <name>`、`killall`、`pkill`、`taskkill /IM` 等命令；",
					"- 进程管理必须精确到 PID：仅允许通过具体 PID（`Stop-Process -Id $PID` 或 `taskkill /PID $PID`）定向终止由当前会话主动启动的子进程；严禁操作任何非自身派生的宿主与系统公共进程。",
					"- 不要执行破坏性命令（如递归删除、格式化磁盘等），它们会被强制要求用户确认。",
				),
				Active: func(Context) bool { return true },
			}},
		},
	}
}

// Item 对外目录条目（前端设置界面直接渲染；Name 由 Variants[0].Title 派生）。
type Item struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Phase         int           `json:"phase"`
	PhaseLabel    string        `json:"phase_label"`
	Category      string        `json:"category"`
	CategoryLabel string        `json:"category_label"`
	Scope         string        `json:"scope"`
	ScopeLabel    string        `json:"scope_label"`
	IsHardRule    bool          `json:"is_hard_rule"`
	Description   string        `json:"description"`
	DisableEffect string        `json:"disable_effect"`
	Variants      []VariantView `json:"variants"`
}

// VariantView 单个变体的对外视图（Text 为提示词真实原文，不含序号）。
type VariantView struct {
	Label string `json:"label"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

// Catalog 返回全局 SOP 数据字典（前后端单一真源）。
func Catalog() []Item {
	defs := Definitions()
	out := make([]Item, 0, len(defs))
	for _, def := range defs {
		variants := make([]VariantView, 0, len(def.Variants))
		for _, v := range def.Variants {
			variants = append(variants, VariantView{Label: v.Label, Title: v.Title, Text: v.Body})
		}
		out = append(out, Item{
			ID:            def.Item.ID,
			Name:          def.Variants[0].Title,
			Phase:         def.Item.Phase,
			PhaseLabel:    def.Item.PhaseLabel,
			Category:      def.Item.Category,
			CategoryLabel: def.Item.CategoryLabel,
			Scope:         def.Item.Scope,
			ScopeLabel:    def.Item.ScopeLabel,
			IsHardRule:    def.Item.IsHardRule,
			Description:   def.Item.Description,
			DisableEffect: def.Item.DisableEffect,
			Variants:      variants,
		})
	}
	return out
}
