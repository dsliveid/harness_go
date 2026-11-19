// Package resultfmt 是工具结果的「入库定型器」。
//
// # 为什么需要这一层
//
// 工具输出在进入 messages 表后会**终生不可变**：每一轮请求都会把全部历史
// 重新发给模型。因此一条失控的工具输出（巨型控制台日志、大文件全文）不是
// 一次性成本，而是**每一轮都重新计费**——成本随轮次线性累加。
//
// 真实事故（会话 9ee34e00）：63 条 tool 消息共 82,194 字符原样落库，单条
// 最长 10,634 字符；最终 input token 累积到 1,090,444，是 output（29,144）
// 的 37 倍。这不是"跑得久"，而是**每条工具输出都在被反复重发**。
//
// # 设计契约（务必保持）
//
//  1. **只定型一次**：定型只发生在「首次入库前」。入库后的内容永不再改写，
//     否则会出现"历史消息每轮都变"的诡异现象，破坏缓存命中与可复现性。
//  2. **保留首尾**：头部是命令/文件的开头（最常被读的部分），尾部是错误与
//     结论（最需要被看到的部分）。掐掉中间，而不是简单截断尾部。
//  3. **必须自我说明**：定型后要在文本里写明"原始多少字符、在哪里看全文"，
//     让模型知道信息被收敛了，而不是误以为这就是全部。
//  4. **零价值内容零改动**：短结果不处理。无谓地改写小输出只会制造噪音与
//     边界 bug（UTF-8 截断、行号错位等）。
//
// # 与老版本的关系
//
// 本包的设计源自 harness_mini 的 context/ingress_normalizer.rs，那套策略在
// 长会话中已实战验证。这里重写为「策略表驱动」：新增工具只加一行 Profile，
// 不改任何逻辑分支——这是与老版本 if/else 链的结构性差异。
package resultfmt

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Profile 描述某个工具输出的收敛策略。
//
// 全部字段为「字符数」而非字节数：工具输出是给人看与给模型读的文本，
// 按字节裁剪会把中文/emoji 切成半个字符，产生非法 UTF-8。
type Profile struct {
	// MaxChars 触发收敛的阈值（<=0 表示该工具不收敛）。
	MaxChars int
	// HeadChars 收敛时保留的头部字符数。
	HeadChars int
	// TailChars 收敛时保留的尾部字符数（0 = 只保留头部）。
	TailChars int
	// MaxLines 行数阈值（0 = 不按行收敛）。与 MaxChars 是「或」关系：
	// grep/list 类工具的输出行数比字符数更能反映信息密度。
	MaxLines int
	// KeepLines 按行收敛时保留的行数（取前 KeepLines 行）。
	KeepLines int
	// Hint 收敛后追加的提示语（告诉模型如何取全量）。
	Hint string
}

// profiles 是工具名 → 策略的映射表。
//
// 阈值取值的依据（均来自事故数据，不是拍脑袋）：
//   - run_command 4000：控制台日志噪声最大，且模型通常只关心开头几行与尾部报错。
//   - grep 40 行：命中行本身信息密度高，前 30 行足以判断方向。
//   - read_file 64K：与项目既有的单次读取上限（maxReadBytes 512K 的 1/8）对齐，
//     同时保证正常文件（几千行）完全不受影响。
//   - list_dir / glob 50 行：目录条目同质化严重，40 项足够定位。
//   - browser 8000：页面文本通常远超实际需要，前 5000 字符覆盖主体。
//
// 未列出的工具走 defaultProfile（宽松 32K），保证新增工具默认有保护，
// 但也绝不会把正常输出切碎。
var profiles = map[string]Profile{
	"run_command": {
		MaxChars: 4000, HeadChars: 2000, TailChars: 1200,
		Hint: "输出过长已收敛（保留首尾核心）。如需完整日志请分段读取或用 grep 定位关键字",
	},
	"grep": {
		MaxLines: 40, KeepLines: 30, MaxChars: 3500,
		Hint: "命中较多已收敛（保留前 30 行）。请缩小 pattern 范围或指定 path 精确检索",
	},
	"read_file": {
		MaxChars: 64 << 10, HeadChars: 32000, TailChars: 16000,
		Hint: "文件过大已收敛首尾。需阅读特定区域请指定 offset_line 与 max_lines",
	},
	"list_dir": {
		MaxLines: 50, KeepLines: 40, MaxChars: 3500,
		Hint: "条目较多已收敛（保留前 40 项）。请改用更具体的子目录路径",
	},
	"glob": {
		MaxLines: 50, KeepLines: 40, MaxChars: 3500,
		Hint: "匹配项较多已收敛（保留前 40 项）。请缩小模式范围（如加上具体目录前缀）",
	},
	"browser_text": {
		MaxChars: 8000, HeadChars: 5000,
		Hint: "页面文本过长已收敛（保留前 5000 字符）",
	},
	"browser_interact": {
		MaxChars: 8000, HeadChars: 5000,
		Hint: "页面文本过长已收敛（保留前 5000 字符）",
	},
}

// defaultProfile 未登记工具的兜底策略。
//
// 必须有默认值：新增工具若忘记登记，不能因此完全没有保护；但阈值放宽到
// 32K，避免把正常输出切碎（宁可漏保护，也不要错误地截断正确内容）。
var defaultProfile = Profile{
	MaxChars: 32 << 10, HeadChars: 20000, TailChars: 8000,
	Hint: "输出较大已收敛首尾",
}

// ProfileFor 返回工具对应的策略（未登记工具返回默认策略）。
func ProfileFor(toolName string) Profile {
	if p, ok := profiles[toolName]; ok {
		return p
	}
	return defaultProfile
}

// Format 按工具策略对输出做入库定型。
//
// 返回值恒为**合法 UTF-8**：所有裁剪都按 rune 进行，绝不切到半个字符。
// 调用方可直接把返回值写入 messages.content。
func Format(toolName, content string) string {
	if content == "" {
		return content
	}
	p := ProfileFor(toolName)
	if p.MaxChars <= 0 && p.MaxLines <= 0 {
		return content
	}

	// 先按行收敛（若策略声明了行阈值）：行级收敛对 grep/list 这类结果是
	// 更自然的边界，不会把某一行切成两半。
	if p.MaxLines > 0 && p.KeepLines > 0 {
		lines := strings.Split(content, "\n")
		if len(lines) > p.MaxLines {
			kept := strings.Join(lines[:p.KeepLines], "\n")
			return fmt.Sprintf("%s\n\n…[%s（共 %d 行，已保留前 %d 行，原始 %d 字符）]",
				kept, p.Hint, len(lines), p.KeepLines, utf8.RuneCountInString(content))
		}
		// 行数未超阈值，继续走字符级判定（可能单行就极长）。
	}

	if p.MaxChars > 0 && utf8.RuneCountInString(content) > p.MaxChars {
		return collapseChars(content, p)
	}
	return content
}

// collapseChars 保留头部 HeadChars 与尾部 TailChars，掐掉中间。
func collapseChars(content string, p Profile) string {
	runes := []rune(content)
	total := len(runes)

	head := p.HeadChars
	if head > total {
		head = total
	}
	tail := p.TailChars
	if tail < 0 {
		tail = 0
	}
	// 头尾之和不得超过总长，否则会重复输出同一段内容。
	if head+tail > total {
		tail = total - head
	}

	var b strings.Builder
	b.WriteString(string(runes[:head]))
	b.WriteString(fmt.Sprintf("\n\n…[%s（原始 %d 字符）]…\n\n", p.Hint, total))
	if tail > 0 {
		b.WriteString(string(runes[total-tail:]))
	}
	return b.String()
}
