package agent

// 工具级「连续失败 / 重复调用」追踪与自纠提示。
//
// # 为什么必须有这一层（2026-11 事故取证）
//
// 事故会话 9ee34e00（「分析项目为什么启动变慢」）的 tool_events 时间线：
//
//	11:12:44 run_command go build  → 失败，耗时 85s（需下载依赖）
//	11:17:08 run_command go build  → 失败，耗时 88s（同一命令，参数几乎相同）
//	11:18:08 run_command go run    → 失败，耗时 98s（同上）
//	11:20:09 run_command go build  → 成功，耗时 12s
//
// 三次同类失败共烧掉 271 秒，占整轮 13.7 分钟的三分之一。模型没有被任何机制
// 告知「你已经失败过两次、且参数几乎一模一样」，只能靠运气在第四次撞对。
//
// 老版本 harness_mini（src-tauri/src/agent.rs:35 ToolErrorTracker）对此有完整实现：
//   - 同一工具连续失败计数，超限（max_retry_limit=2）后注入中文自纠提示；
//   - 提示明确写「严禁不作分析直接重复调用相同参数！」；
//   - 并按工具名给出**具体**建议（edit_file → 先 read_file 复核原文；
//     run_command → 阅读 stderr、检查 Windows PowerShell 语法兼容性）。
//
// 本文件把它移植到 Go 侧，并额外补上「参数完全重复」的拦截（老版本只统计失败，
// 不检测重复调用；而重复调用在成功路径上同样是纯浪费）。
//
// # 作用域
//
// 追踪器必须**按 Run 隔离**（每次 loop 一个实例），不能放 Engine 字段：
// Engine 是全进程单例、8 路并发共享，共享追踪器会把 A 会话的失败算到 B 会话头上
// （与 maxCompactionsPerRun 必须放 loop 局部是同一个理由，见 engine.go:94 注释）。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"harness-go/internal/model"
)

// maxConsecutiveToolFailures 同一工具允许的连续失败次数上限。
//
// 取 2 与老版本一致：第一次失败允许重试（可能是偶发），第二次仍失败就必须
// 明确告知模型「换个策略」，第三次开始直接拦下。
const maxConsecutiveToolFailures = 2

// maxIdenticalCalls 同一 (工具名 + 归一化参数) 完全重复的调用次数上限。
//
// 重复调用**即便成功**也是纯浪费（结果已在上下文里）。给 2 而不是 1：
// 允许「同参数重跑一次以确认非偶发」这一合法用法，第三次必须拦。
const maxIdenticalCalls = 2

// toolErrorTracker 单 Run 内的工具调用健康度追踪器。
type toolErrorTracker struct {
	// failures 工具名 → 连续失败次数（成功即清零）。
	failures map[string]int
	// calls 调用指纹（工具名+归一化参数哈希）→ 累计调用次数。
	calls map[string]int
	// count 本 Run 累计调用次数（仅用于提示语中的「第 N 次」）。
	count int
}

func newToolErrorTracker() *toolErrorTracker {
	return &toolErrorTracker{
		failures: make(map[string]int),
		calls:    make(map[string]int),
	}
}

// fingerprint 生成调用指纹：工具名 + **规范化**参数。
//
// 归一化分两层：
//  1. 合法性（复用 normalizeArgs：非法 JSON 先包成字符串，连这都失败退化 {}）；
//  2. 形状（unmarshal → marshal）：消除缩进/空格/键序差异。
//
// 否则模型只要改一个空格就能绕过重复检测 —— 那在语义上是同一个调用。
func fingerprint(name string, args []byte) string {
	norm := normalizeArgs(args)
	var v any
	if err := json.Unmarshal(norm, &v); err == nil {
		if b, mErr := json.Marshal(v); mErr == nil {
			norm = b
		}
	}
	h := sha256.Sum256([]byte(name + "\x00" + string(norm)))
	return hex.EncodeToString(h[:8])
}

// allow 判断本次调用是否放行；拒绝时返回给模型的提示文案。
//
// 返回 (allowed, notice)。notice 非空时**必须**作为该次工具结果回灌模型，
// 让模型知道"为什么被拦"，而不是收到一个无法解释的空结果。
func (t *toolErrorTracker) allow(name string, args []byte) (bool, string) {
	t.count++
	key := fingerprint(name, args)
	t.calls[key]++

	if t.calls[key] > maxIdenticalCalls {
		return false, toolDuplicateNotice(name, t.calls[key])
	}
	// 连续失败已在「超限」态：不允许继续用同一工具盲试（换工具或换参数才放行）。
	if n := t.failures[name]; n > maxConsecutiveToolFailures {
		return true, toolFailureWarning(name, n)
	}
	return true, ""
}

// recordSuccess 记录成功（清零该工具的失败计数）。
func (t *toolErrorTracker) recordSuccess(name string) {
	delete(t.failures, name)
}

// recordFailure 记录失败并返回 (是否已超限, 连续失败次数)。
func (t *toolErrorTracker) recordFailure(name string) (bool, int) {
	t.failures[name]++
	n := t.failures[name]
	return n > maxConsecutiveToolFailures, n
}

// toolReflexionAdvice 按工具名给出**具体**自纠建议（照搬老版本的分支口径）。
//
// 泛泛的「请检查参数」对模型几乎无引导作用 —— 老版本的实测经验是：
// 给出该工具最常见的失败原因，模型当轮就能改对。
func toolReflexionAdvice(name string) string {
	switch name {
	case "edit_file":
		return "目标文本未匹配到。请务必先调用 `read_file` 重新读取该文件相关代码行，" +
			"获取准确的缩进、换行与上下文后再发起编辑。"
	case "read_file", "glob", "list_dir":
		return "文件或路径不存在。请检查路径拼写，或先用 `glob` 搜索工作区以确认真实相对路径。"
	case "run_command", "run_skill":
		return "命令执行报错。请仔细阅读错误流（stderr），检查命令参数、依赖是否已下载，" +
			"以及是否混用了不兼容的 shell 语法（cmd / PowerShell）。"
	case "todo", "update_plan":
		return "参数不符合契约。请先调用 `lookup_tool` 取到完整参数规范再调用，不要猜参数名。"
	default:
		return "请检查工具参数是否符合规范，修正后重新尝试。"
	}
}

// toolFailureWarning 连续失败超限的警告文案（不阻断，但强引导换策略）。
func toolFailureWarning(name string, n int) string {
	return "\n\n【⚠️ 工具调用错误自纠提示（已连续失败 " + itoa(n) + " 次）】\n" +
		"工具 `" + name + "` 反复失败。\n" +
		"【自纠建议】: " + toolReflexionAdvice(name) + "\n" +
		"请根据上述原因，自主分析并**改变策略**（换工具 / 换参数 / 先勘察再重试）；" +
		"严禁不作分析直接重复调用相同参数！"
}

// toolDuplicateNotice 完全重复调用的拦截文案（不执行，直接回灌）。
func toolDuplicateNotice(name string, n int) string {
	return "【调用已被拦截】工具 `" + name + "` 使用**完全相同的参数**已调用 " + itoa(n) + " 次。\n" +
		"重复调用不会产生新信息（结果已在上文中）。请改为：\n" +
		"1. 从已有结果中读取结论；或\n" +
		"2. 改变参数范围（更精确的路径 / 条件 / 命令）后再试；或\n" +
		"3. 换用其它工具达成同一目标。\n" +
		"【自纠建议】: " + toolReflexionAdvice(name)
}

// itoa 整数字面量转字符串（提示文案拼接用）。
func itoa(n int) string { return strconv.Itoa(n) }

// isToolFailure 判断一条 tool 消息是否代表**执行失败**（用于失败计数）。
//
// 为什么按文案前缀判定而不是让 execTool 返回错误标志：
// model.Message 没有 is_error 列，而改动存储 schema 的代价远大于收益；
// 且这几种前缀本身就在 execTool 内唯一产生（见 execTool 的各个 return 点），
// 是一份稳定契约。若将来新增失败文案，必须同步加到这里——所以集中在一处。
func isToolFailure(m *model.Message) bool {
	if m == nil || m.Role != model.RoleTool {
		return false
	}
	for _, p := range []string{
		"工具执行失败:",
		"工具事件落库失败:",
		"审批登记失败:",
		"审批状态迁移失败:",
		"审批后状态回迁失败:",
		"denied:",
	} {
		if strings.HasPrefix(m.Content, p) {
			return true
		}
	}
	return false
}
