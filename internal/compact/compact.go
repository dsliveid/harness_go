// Package compact 实现上下文压缩（compaction）的生命周期编排。
//
// # 为什么这一层必须存在
//
// 仓库里原本已有「五级降级」链路（internal/prompt/budget.go），其中第五级
// LevelForceCompress 的注释写着「需外部便宜模型，本层只产出信号」——
// 也就是说**信号发出了，但没有任何消费者**。同时 budget.max_total 默认 0
// （不限），整条降级链从不触发。机制存在但常年休眠。
//
// 真实事故（会话 9ee34e00）：单 Run 内 input token 累积 1,090,444，output 仅
// 29,144（37 倍）。该会话全程 49 次工具调用、只有 1 条 user 消息，属于典型的
// 「单轮超长任务」——这类会话最容易打爆上下文，且无法靠「砍最旧消息」解决
// （那会把当前任务的前提整段砍掉）。
//
// # 与 resultfmt 的分工
//
//   - resultfmt 治「单条太大」：在入库前把一条失控输出收敛到策略上限；
//   - compact 治「条数太多」：把已完成的旧轮次折叠成摘要。
//
// 二者是正交的，缺任何一个都堵不住成本。定型后单条变小，但 100 轮累计仍会超窗。
//
// # 设计来源
//
// 核心思路来自 harness_mini 的 context/governance.rs（已在长会话实战验证）：
//   - 80% 水位触发（不给模型"刚好打满"的机会）；
//   - 活跃轮次保护：运行中只压缩「最后一个 user 消息之前」的部分；
//   - 持久化摘要 + end_seq 边界，而不是丢弃消息（原始记录始终可回放）。
//
// 但这里做了一处**结构性改进**：把「算什么」与「压什么」彻底分开。
// Evaluate 只做纯计算（可独立单测、无 IO），Plan 只做纯分区（无 IO），
// Service 才碰存储与模型。老版本把三者揉在一个长函数里，导致水位逻辑
// 无法单独验证——这正是它需要大段注释解释的原因。
package compact

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"harness-go/internal/llm"
	"harness-go/internal/model"
)

// ThresholdPercent 触发压缩的水位百分比。
//
// 为什么是 80 而不是 95：压缩本身是一次额外的模型调用（要花时间与钱）。
// 若等到 95% 才压，压缩期间的新增输出可能直接把窗口打爆，变成硬失败。
// 留 20% 余量是为了让压缩在「还有空间」时从容完成。
const ThresholdPercent = 80

// Viewer 是压缩视图：描述「当前上下文占用了多少、还能装多少」。
type Viewer struct {
	// Used 当前已占用的估算 token。
	Used int
	// Limit 上下文窗口上限。
	Limit int
	// Threshold 触发压缩的水位线（= Limit * 80%）。
	Threshold int
	// NeedsCompaction 是否已达水位线。
	NeedsCompaction bool
	// Percentage 水位百分比（一位小数，供 UI 展示）。
	Percentage float64
}

// Evaluate 计算水位视图（纯函数，无 IO）。
//
// 调用方负责提供 Used（通常来自 prompt.BuildResult.Breakdown.Total）与 Limit
// （来自模型元数据）。本函数不猜测、不估算，只做阈值算术——这样它在任何
// 场景下都可用，且行为完全可预测。
func Evaluate(used, limit int) Viewer {
	if limit <= 0 {
		// 窗口未知时不压缩：宁可让预算闸门去报 budget_exhausted（那是有信号
		// 的失败），也不要基于猜测的窗口做破坏性折叠。
		return Viewer{Used: used}
	}
	threshold := limit * ThresholdPercent / 100
	pct := 0.0
	if limit > 0 {
		pct = float64(used) / float64(limit) * 100
		// 只保留一位小数，避免浮点噪声进入日志与 UI。
		pct = float64(int(pct*10)) / 10
	}
	return Viewer{
		Used:            used,
		Limit:           limit,
		Threshold:       threshold,
		NeedsCompaction: used >= threshold,
		Percentage:      pct,
	}
}

// Plan 是一次压缩的边界规划结果。
type Plan struct {
	// StartSeq 被压缩区间的起点（含）。
	StartSeq int64
	// EndSeq 被压缩区间的终点（含）。
	EndSeq int64
	// Messages 将被折叠的消息（按 seq 升序）。
	Messages []model.Message
	// EstimatedTokens 这批消息的估算 token（用于统计压缩收益）。
	EstimatedTokens int
}

// Empty 判断本次规划是否有可压缩内容。
func (p Plan) Empty() bool { return len(p.Messages) == 0 }

// PlanCompaction 规划压缩区间（纯函数，无 IO）。
//
// # 最关键的规则：活跃轮次保护
//
// msgs 是全量消息（按 seq 升序），alreadyCompactedEndSeq 是已压缩过的最大 seq。
// 只有当存在「已完成的轮次」时才压缩：
//
//   - running == true（本 Run 正在执行）：当前轮的起点是最后一条 user 消息。
//     它之后的一切都属于"正在进行的任务"，折叠掉就等于让模型失忆。
//     因此只压 [lastUserIdx, 末尾) 之前的部分。
//   - running == false（空闲态）：末轮已收工，整段历史都可作为"已完成的工件"
//     被总结。这正是事故会话那种「1 条 user + 47 轮工具」的场景——
//     它已完成（或被取消），可以整段折叠。
//
// # 为什么不按条数阈值压
//
// 「保留最近 N 条」在工具密集会话里是错的：最近 20 条可能只是 20 次 grep，
// 真正重要的任务前提在更早的位置。按「轮次边界」切才符合语义。
func PlanCompaction(msgs []model.Message, alreadyCompactedEndSeq int64, running bool) Plan {
	// 只考虑尚未压缩、且未置灰的消息。
	candidates := make([]model.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Seq <= alreadyCompactedEndSeq {
			continue
		}
		if m.Hidden {
			continue
		}
		candidates = append(candidates, m)
	}
	if len(candidates) == 0 {
		return Plan{}
	}

	boundary := len(candidates) // 折叠 [0, boundary)
	if running {
		// 找最后一条 user 消息作为当前轮起点。
		lastUser := -1
		for i := len(candidates) - 1; i >= 0; i-- {
			if candidates[i].Role == model.RoleUser {
				lastUser = i
				break
			}
		}
		switch {
		case lastUser > 0:
			boundary = lastUser
		case len(candidates) >= 2:
			// 🚨 2026-11 缺陷修复（「机制第二次休眠」，勿回退）
			//
			// 旧实现在这里分两支：`lastUser == 0` → **直接 return Plan{}**（永不压缩）。
			// 那个分支把「当前轮占满整个未压缩区间」当成了「没有任何可折叠内容」，
			// 而它恰恰是**最该压缩**的形态：
			//   - 单轮超长任务（1 条 user + 上百条工具步骤）：lastUser 恒为 0；
			//   - 会话 122559a3 的第 2 个 Run：未压缩区间从 seq42 起，若历史只读到
			//     区间头部（旧 loadHistory 被静默截断为最旧 100 条）也会退化成 lastUser==0。
			// 结果：单会话账单烧到 839 万 token，而 session_compactions 始终为 0。
			//
			// 正确语义对齐老版本 harness_mini governance.rs:361-373：
			// 找不到可切的轮次边界时，退化为「保留尾部一个完整现场、折叠其余全部」——
			// 任务背景由结构化摘要承接（摘要 prompt 明确要求保留任务目标与关键决策），
			// 模型不会因此失忆，但上下文规模能真正降下来。
			//
			// 「尾部一个完整现场」= 最后一条非 tool 消息及其后续 tool 结果：
			// 若只保留最后一条且它是 tool，它的 declaring assistant 已被折进摘要
			// → 重放时出现孤儿 tool，上游必然 400。故必须从尾往前找到非 tool 起点。
			keepFrom := len(candidates) - 1
			for keepFrom > 0 && candidates[keepFrom].Role == model.RoleTool {
				keepFrom--
			}
			boundary = keepFrom
		default:
			// 未压缩区间只有 1 条：确实没有可折叠内容（压缩收益必然为负）。
			return Plan{}
		}
	}

	if boundary <= 0 {
		return Plan{}
	}
	// 压缩区间必须完整覆盖到边界前一条，且末条不能是「悬空的 tool_calls
	// 声明」——否则重放时会出现 assistant(tool_calls) 后面没有对应 tool 结果，
	// 上游会直接 400（历史里已有 sanitizeToolCalls 处理同类污染，这里从源头避免）。
	boundary = trimTrailingDanglingCalls(candidates, boundary)
	if boundary <= 0 {
		return Plan{}
	}

	picked := candidates[:boundary]
	return Plan{
		StartSeq:        picked[0].Seq,
		EndSeq:          picked[len(picked)-1].Seq,
		Messages:        picked,
		EstimatedTokens: estimateMessages(picked),
	}
}

// trimTrailingDanglingCalls 把边界向前收缩，确保被折叠区间末尾不是
// 「带 tool_calls 但没有对应 tool 结果」的 assistant 消息。
//
// 这个边界细节很容易被忽略，但后果严重：若折叠区间把 assistant(tool_calls)
// 包进去、却把配对的 tool 结果留在外面，重建历史时会出现工具调用声明丢失，
// 上游 provider 会拒绝整个请求。
func trimTrailingDanglingCalls(msgs []model.Message, boundary int) int {
	// 从 boundary-1 往回找：若末条是 assistant 且带 tool_calls，
	// 则必须继续向前，直到该 assistant 之后的 tool 结果也被包含进来。
	// 由于 boundary 之前全部会被折叠，只要末条 assistant 的 tool_calls
	// 数量与它后面（同一区间内）的 tool 结果数量不匹配，就向前收缩到该
	// assistant 之前。
	for boundary > 0 {
		last := msgs[boundary-1]
		if last.Role != model.RoleAssistant || len(last.ToolCallsJSON) == 0 {
			return boundary
		}
		// 末条是带工具调用的 assistant，但它的 tool 结果在区间之外 → 收缩。
		boundary--
	}
	return 0
}

// estimateMessages 粗略估算一批消息的 token（用于统计，不用于决策）。
//
// 与 prompt 层的估算口径保持独立：这里只需要"压缩掉多少"的相对量级，
// 引入 turnkey 计数器反而会增加耦合。真正的预算判断一律由 prompt.Builder
// 重新计数（那是唯一权威）。
func estimateMessages(msgs []model.Message) int {
	total := 0
	for _, m := range msgs {
		total += estimateText(m.Content) + estimateText(m.Reasoning)
		total += len(m.ToolCallsJSON) / 4
	}
	return total
}

// estimateText 按字符类型粗估 token（CJK 1:1，其余 4:1）。
func estimateText(s string) int {
	if s == "" {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range s {
		if r >= 0x2E80 {
			cjk++
		} else {
			other++
		}
	}
	return cjk + other/4
}

// BuildSummaryPrompt 生成压缩所用的提示词（纯函数）。
//
// # 为什么提示词要写得这么"死"
//
// 摘要会成为后续所有轮次的上下文前提。若摘要丢了「用户到底想干什么」，
// 模型后续所有回答都会跑偏，而且这种跑偏是**静默的**——用户只看到答非所问，
// 完全不知道为什么。因此提示词强制要求保留：目标、已确认事实、已排除方向、
// 待办事项四类信息。
//
// # 结构与老版本对齐（harness_mini governance.rs:594-611）
//
// 老版本用四段式 Markdown（🎯任务背景 / 🔑技术决策 / 📁涉及文件 / 📌遗留事项），
// 实测效果显著优于自由格式：模型后续能按段落定位"我要的那个信息在哪"。
// 本项目此前是「四条自然语言要求」，模型经常把四条揉成一段散文，可读性差。
func BuildSummaryPrompt(turns []model.Message) string {
	var b strings.Builder
	b.WriteString("你是编码项目的上下文备忘助手。请把下面这段已完成的对话精炼成结构化摘要，")
	b.WriteString("它将在后续轮次替代这段历史，用来保持关键记忆。\n\n")
	b.WriteString("必须保留（缺一不可）：\n")
	b.WriteString("1. 用户原始目标的准确表述（不要改写、不要泛化）；\n")
	b.WriteString("2. 已经确认的事实与结论（含关键文件路径、行号、命令结论）；\n")
	b.WriteString("3. 已经排除的错误方向及排除原因（避免后续重复踩坑）；\n")
	b.WriteString("4. 尚未完成的事项，以及下一步该做什么。\n\n")
	b.WriteString("请严格按以下结构输出 Markdown（简洁干练，剔除寒暄与重复过程）：\n")
	b.WriteString("### 🎯 任务背景与核心目标\n")
	b.WriteString("### 🔑 关键技术决策与约定\n")
	b.WriteString("### 📁 涉及文件与修改记录\n")
	b.WriteString("### 📌 历史遗留与注意事项\n\n")
	b.WriteString("可以丢弃：完整的工具原始输出、重复的中间过程、与目标无关的探索。\n")
	b.WriteString("不要编造对话中未出现的信息。若某项在原对话中没有出现，该段落写「（无）」即可。\n\n")
	b.WriteString("输出要求：Markdown，不超过 800 字。\n\n")
	b.WriteString("---- 以下是待压缩的对话 ----\n")
	for _, m := range turns {
		switch m.Role {
		case model.RoleUser:
			b.WriteString("\n[用户] " + m.Content + "\n")
		case model.RoleAssistant:
			if strings.TrimSpace(m.Content) != "" {
				b.WriteString("\n[助手] " + m.Content + "\n")
			}
			if len(m.ToolCallsJSON) > 0 {
				b.WriteString("[助手调用了工具]\n")
			}
		case model.RoleTool:
			// 工具输出是噪声主体，只留极短摘录，避免把待压缩内容本身撑爆。
			b.WriteString("[工具结果] " + truncate(m.Content, 200) + "\n")
		}
	}
	return b.String()
}

// truncate 按 rune 截断，保证不产生非法 UTF-8。
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// Summarizer 把一段对话折叠成摘要（由上层注入具体模型实现）。
//
// 声明为接口而不是直接依赖 llm.Provider：压缩可以用更便宜的模型，
// 也可以被替换成纯规则实现（测试、离线场景）。这是"压缩策略"与
// "模型调用"解耦的关键。
type Summarizer interface {
	Summarize(ctx context.Context, prompt string) (string, error)
}

// SummarizerFunc 适配函数为 Summarizer。
type SummarizerFunc func(ctx context.Context, prompt string) (string, error)

// Summarize 实现 Summarizer。
func (f SummarizerFunc) Summarize(ctx context.Context, prompt string) (string, error) {
	return f(ctx, prompt)
}

// History 把压缩记录叠加到消息列表上，得到"模型实际看到的历史"。
//
// # 这是压缩对读取侧的**唯一**入口
//
// 凡是要把历史消息喂给模型的地方，都必须经过本函数。这样「已压缩区间被
// 摘要替代」这条语义只有一处实现，不会出现「某个调用点忘了应用压缩」的
// 静默不一致。
//
// 输出顺序：按时间线插入。每个压片区间的摘要在其 end_seq 处出现。
func History(msgs []model.Message, compactions []model.Compaction) []llm.Message {
	if len(compactions) == 0 {
		out := make([]llm.Message, 0, len(msgs))
		for _, m := range msgs {
			out = append(out, toLLM(m))
		}
		return out
	}
	out := make([]llm.Message, 0, len(msgs)+len(compactions))
	// 按 end_seq 升序消费压缩区间。
	ci := 0
	for _, m := range msgs {
		// 先把所有 end_seq < 当前消息 seq 的摘要吐出去（时间线顺序正确）。
		for ci < len(compactions) && compactions[ci].EndSeq < m.Seq {
			out = append(out, summaryMessage(compactions[ci]))
			ci++
		}
		// 落在某个压缩区间内的消息被摘要替代。
		if ci < len(compactions) && m.Seq <= compactions[ci].EndSeq {
			// 该区间的摘要已在上面或此处输出；跳到区间末尾。
			for ci < len(compactions) && compactions[ci].EndSeq < m.Seq {
				ci++
			}
			if ci < len(compactions) && m.Seq >= compactions[ci].StartSeq {
				continue
			}
		}
		out = append(out, toLLM(m))
	}
	// 收尾：剩余摘要在末尾（其 end_seq 大于所有消息 seq 的极端情况）。
	for ; ci < len(compactions); ci++ {
		out = append(out, summaryMessage(compactions[ci]))
	}
	return out
}

// summaryMessage 把一条压缩记录表示成"系统视角的历史消息"。
//
// 用 user 角色而非 system：摘要代表"此前发生过的事实"，应当作为对话内容
// 的一部分被模型理解，而不是作为指令。用 system 会让模型误以为这是新的约束。
func summaryMessage(c model.Compaction) llm.Message {
	return llm.Message{
		Role: llm.RoleUser,
		Content: fmt.Sprintf(
			"[此前对话摘要]（已折叠第 %d–%d 条消息，共 %d 条）\n\n%s",
			c.StartSeq, c.EndSeq, c.ReplacedMessages, c.SummaryMarkdown),
	}
}

// toLLM 把持久化消息转换为模型消息。
//
// 与 agent.toLLMMessage 保持同一口径：tool_calls_json 反序列化失败时
// **静默降级为无工具调用**（而不是报错）——因为历史里已存在被 sanitize
// 兜底过的记录，读取侧必须能容忍它们，否则整个会话将永久无法加载。
func toLLM(m model.Message) llm.Message {
	out := llm.Message{
		Role:       llm.Role(m.Role),
		Content:    m.Content,
		ToolCallID: m.ToolCallID,
	}
	if len(m.ToolCallsJSON) > 0 {
		var tcs []llm.ToolCall
		if err := json.Unmarshal(m.ToolCallsJSON, &tcs); err == nil {
			out.ToolCalls = tcs
		}
	}
	return out
}
