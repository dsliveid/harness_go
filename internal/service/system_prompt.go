package service

import "strings"

// ComposeSystemBase 把「基础系统提示」与「本会话真实工作区事实」组装为最终 SystemBase。
//
// 背景（踩坑）：基础提示是启动期固定常量，与会话级 project_dir 完全解耦，
// 曾出现「会话顶栏显示纯对话·未绑定工作区，模型却自称运行在用户的工作区内」的幻觉。
// 会话是否绑定工作区属于**运行时事实**，必须按会话注入，不能写死在基础提示里。
//
// 组装口径：
//   - 已绑定 → 明确目录（文件工具相对路径以此为根），禁止改口到别的目录；
//   - 未绑定 → 明确声明纯对话，禁止声称已绑定工作区或已在某项目目录内运行；
//     workDir（服务端进程目录）如实披露为工具落盘边界，不伪装成用户绑定。
func ComposeSystemBase(base, workspacePath, workDir string) string {
	ws := strings.TrimSpace(workspacePath)
	wd := strings.TrimSpace(workDir)

	var sb strings.Builder
	sb.WriteString("\n\n## 工作区\n")
	if ws != "" {
		sb.WriteString("本会话已绑定工作区：" + ws + "\n")
		sb.WriteString("文件工具的相对路径以此为根；不要声称在其他目录内运行，也不要越界读写工作区之外的文件。")
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
