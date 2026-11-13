package service

import (
	"strings"
	"testing"
)

// ============ SystemBase 工作区事实注入（提示词与执行根对齐）============
//
// 背景（2026-11 修正）：此前对已绑定会话无条件断言「文件工具相对路径以 project_dir
// 为根」，但工具实际以进程启动目录为根。当 project_dir ≠ workDir 时，这句话是**假的**，
// 模型会基于错误前提在错误目录里读写 —— 属于最隐蔽的一类缺陷（不报错、结果错）。
//
// 现在执行根已真正跟随 project_dir（execution_root.go + WithWorkspaceRoot），
// 因此本组测试既要锁住「文案与真实行为一致」，也要锁住「未绑定时不得谎称已绑定」。

// ① 已绑定且与进程目录不同 → 明确声明执行根 + 澄清两者区别。
func TestComposeSystemBaseBoundDifferentFromWorkDir(t *testing.T) {
	out := ComposeSystemBase("BASE", `F:\proj\demo`, `F:\WorkSpace\Other\harness_go`)

	if !strings.Contains(out, `F:\proj\demo`) {
		t.Fatalf("必须声明本会话工作区路径，实际: %s", out)
	}
	if !strings.Contains(out, "所有文件工具的相对路径都以该目录为根") {
		t.Fatalf("必须如实声明执行根口径，实际: %s", out)
	}
	if !strings.Contains(out, "与本会话工作区无关") {
		t.Fatalf("工作区与进程目录不同时必须澄清，避免模型混淆，实际: %s", out)
	}
	if !strings.HasPrefix(out, "BASE") {
		t.Fatalf("基础提示必须被保留在开头，实际: %s", out)
	}
}

// ② 已绑定且两者相同 → 不应出现多余的澄清（避免提示词噪音）。
func TestComposeSystemBaseBoundSameAsWorkDir(t *testing.T) {
	out := ComposeSystemBase("", `F:\proj`, `F:\proj`)
	if strings.Contains(out, "与本会话工作区无关") {
		t.Fatalf("两者相同时不应附加澄清，实际: %s", out)
	}
	if !strings.Contains(out, `F:\proj`) {
		t.Fatalf("仍必须声明工作区路径，实际: %s", out)
	}
}

// ③ 未绑定（纯对话）→ 必须声明纯对话，且不得声称已在项目目录内。
func TestComposeSystemBaseUnbound(t *testing.T) {
	out := ComposeSystemBase("", "", `F:\WorkSpace\Other\harness_go`)

	if !strings.Contains(out, "未绑定工作区") {
		t.Fatalf("未绑定时必须明确声明，实际: %s", out)
	}
	if strings.Contains(out, "所有文件工具的相对路径都以该目录为根") {
		t.Fatalf("未绑定时不得声明会话执行根，实际: %s", out)
	}
	if !strings.Contains(out, `F:\WorkSpace\Other\harness_go`) {
		t.Fatalf("未绑定时应披露进程目录作为兜底边界，实际: %s", out)
	}
}

// ④ 空白路径等同未绑定（防 " " 被当成已绑定）。
func TestComposeSystemBaseBlankIsUnbound(t *testing.T) {
	out := ComposeSystemBase("", "   ", "")
	if !strings.Contains(out, "未绑定工作区") {
		t.Fatalf("空白路径应视为未绑定，实际: %s", out)
	}
}

// ⑥ 空白基础提示 + 空白工作区：只输出工作区段落，不留前导空行。
//
// 对应改造前的 TestComposeSystemBaseTrim（行为契约不变）。
func TestComposeSystemBaseTrim(t *testing.T) {
	got := ComposeSystemBase("   ", "   ", "")
	if strings.HasPrefix(got, "\n") {
		t.Fatalf("不应以空行开头，得到: %q", got)
	}
	if !strings.Contains(got, "未绑定工作区") {
		t.Fatalf("空白工作区按未绑定处理，得到: %q", got)
	}
}

// ⑤ 基础提示为空时不得以换行开头（避免拼进 system prompt 后出现空行噪音）。
func TestComposeSystemBaseEmptyBaseNoLeadingNewline(t *testing.T) {
	out := ComposeSystemBase("", `F:\p`, "")
	if strings.HasPrefix(out, "\n") {
		t.Fatalf("输出不得以换行开头，实际: %q", out[:10])
	}
}
