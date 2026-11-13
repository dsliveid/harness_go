package service

import (
	"path/filepath"
	"runtime"
	"strings"
)

// ComposeSystemBase 把「基础系统提示」与「本会话真实工作区事实」组装为最终 SystemBase。
//
// 背景（踩坑）：基础提示是启动期固定常量，与会话级 project_dir 完全解耦，
// 曾出现「会话顶栏显示纯对话·未绑定工作区，模型却自称运行在用户的工作区内」的幻觉。
// 会话是否绑定工作区属于**运行时事实**，必须按会话注入，不能写死在基础提示里。
//
// 组装口径：
//   - 已绑定 → 明确目录，并声明它**就是**本会话工具的真实执行根；
//   - 未绑定 → 明确声明纯对话，禁止声称已绑定工作区或已在某项目目录内运行；
//     workDir（服务端进程目录）如实披露为工具落盘边界，不伪装成用户绑定。
//
// 📌 2026-11 修正（重要）：此前本函数对已绑定会话断言「文件工具的相对路径以此为根」，
// 但工具实际仍以进程目录为根 —— 当 project_dir ≠ workDir 时，这句话是**假的**，
// 模型会基于错误前提在错误目录里读写。现已让执行根真正跟随 project_dir
// （见 service/execution_root.go + toolapi.WithWorkspaceRoot），故该断言成立。
// 若未来又出现「提示词与载体不一致」，必须先修载体而不是改这段文案。
func ComposeSystemBase(base, workspacePath, workDir string) string {
	ws := strings.TrimSpace(workspacePath)
	wd := strings.TrimSpace(workDir)

	var sb strings.Builder
	sb.WriteString("\n\n## 工作区\n")
	if ws != "" {
		sb.WriteString("本会话已绑定工作区：" + ws + "\n")
		sb.WriteString("本会话所有文件工具的相对路径都以该目录为根，命令也在该目录下执行；" +
			"不要声称在其他目录内运行，也不要越界读写工作区之外的文件。")
		if wd != "" && !sameDir(ws, wd) {
			// 服务端进程目录与工作区不同时，明确区分两者，避免模型把进程目录当成工作区
			// （例如它看到日志里出现工作区路径就以为自己在启动目录里）。
			sb.WriteString("\n（服务端进程自身的启动目录是 " + wd +
				"，与本会话工作区无关，不要把两者混为一谈。）")
		}
	} else {
		sb.WriteString("本会话**未绑定工作区**（纯对话）。即使文件工具可用，也不得声称「已绑定工作区」或「运行在用户的某个项目目录内」。")
		if wd != "" {
			sb.WriteString("\n工具的相对路径落在服务端进程目录：" + wd + "（这不是用户指定的工作区；涉及用户项目路径时先请用户明确目录）。")
		}
	}

	out := strings.TrimSpace(base)
	if out == "" {
		return strings.TrimLeft(sb.String(), "\n")
	}
	return out + sb.String()
}

// sameDir 判断两个路径是否指向同一目录（大小写不敏感由 filepath.Clean 后的
// 逐字比较近似处理；此处仅用于决定是否附加澄清说明，不承担安全判定职责）。
func sameDir(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}
