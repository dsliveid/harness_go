package sop

import (
	"os"
	"path/filepath"
	"strings"
)

// 项目约束摘要：把工作区 `.harness/memory/` 下的大盘/规约压成几行注入 system。
//
// # 为什么需要它
//
// 记忆有两条互补通道：
//   - 内核 memory.Store 走「检索召回」（按相关性打分，进 MemoryCore/MemoryTail）；
//   - `profile.md` / `conventions.md` 是**人手维护的项目大盘**，内容长、变化慢，
//     且不适合参与向量检索 —— 它们是"始终该知道"的背景，不是"这次相关"的命中。
//
// 改造前 `prompt.Input.ProjectSummary` 字段虽有完整渲染逻辑，但调用方
// 硬编码传空串（run_service.go），于是「## 项目约束」段永远不会出现 ——
// 一个设计好却从未被喂过数据的分支。
//
// # 为什么在这里（internal/sop）而不是 service
//
// 本包只依赖标准库（铁律）。读文件 + 截断是纯函数逻辑，放这里可被 service
// 与测试共同复用，且不引入对 store/plugins 的依赖。
//
// # 铁律
//
//   - 只读、绝不创建：缺失文件返回空串（不建空文件、不写占位符）。
//   - 有界：每份文档单独截断，且总长有上限 —— 这是**背景注入**，不是正文，
//     膨胀会直接吃掉每轮预算（而且会随轮次线性叠加成本）。
//   - 按章节边界截断：优先在 "\n## " 处切，避免把一段话拦腰截断产生误读。
const (
	// profileMaxChars profile.md 注入上限（架构大盘）。
	profileMaxChars = 2500
	// conventionsMaxChars conventions.md 注入上限（工程规约）。
	conventionsMaxChars = 1500
)

// LoadProjectSummary 读取工作区项目约束摘要（缺失即空串）。
//
// workDir 为空或无 .harness/memory 时返回空串（纯对话会话无需项目约束）。
func LoadProjectSummary(workDir string) string {
	if strings.TrimSpace(workDir) == "" {
		return ""
	}
	base := filepath.Join(workDir, ".harness", "memory")
	var parts []string
	if s := readTruncated(filepath.Join(base, "profile.md"), profileMaxChars, "### 项目技术大盘与架构档案");
	s != "" {
		parts = append(parts, s)
	}
	if s := readTruncated(filepath.Join(base, "conventions.md"), conventionsMaxChars, "### 工程规范与避坑约定");
	s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n\n")
}

// readTruncated 读取文档并按字符边界安全截断。
//
// ⚠️ 必须按 **rune** 而非 byte 切片：中文 UTF-8 每字 3 字节，用字节下标
// 会落在汉字中间，产生非法字符（历史上同类事故表现为输出乱码）。
func readTruncated(path string, maxChars int, heading string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "" // 缺失是正常态
	}
	text := strings.TrimSpace(strings.ReplaceAll(string(data), "\r\n", "\n"))
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > maxChars {
		cut := string(runes[:maxChars])
		// 尽量回退到最近的章节边界，避免把一句话拦腰截断
		if i := strings.LastIndex(cut, "\n## "); i > 0 {
			cut = cut[:i]
		}
		return heading + "\n" + strings.TrimSpace(cut) +
			"\n\n*(注：内容超长，已截取前部核心，完整内容见 .harness/memory/" +
			filepath.Base(path) + ")*"
	}
	return heading + "\n" + text
}
