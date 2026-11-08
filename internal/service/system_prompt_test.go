package service

import (
	"strings"
	"testing"
)

// TestComposeSystemBaseUnbound 未绑定工作区：必须明确声明纯对话，
// 且不得出现「运行在用户的工作区内」这类与实际绑定状态矛盾的断言。
func TestComposeSystemBaseUnbound(t *testing.T) {
	base := "你是编码助手。"
	got := ComposeSystemBase(base, "", `D:\WorkSpace\Project\harness_go`)

	if !strings.HasPrefix(got, base) {
		t.Fatalf("基础提示应保持在前缀，得到: %q", got)
	}
	if !strings.Contains(got, "未绑定工作区") {
		t.Fatalf("未绑定会话必须显式声明未绑定工作区，得到: %q", got)
	}
	if strings.Contains(got, "运行在用户的工作区内") {
		t.Fatalf("未绑定会话不得声称运行在用户工作区内，得到: %q", got)
	}
	// 进程目录如实披露（作为工具落盘边界），但不能冒充用户绑定目录
	if !strings.Contains(got, "harness_go") {
		t.Fatalf("应如实披露工具落盘的服务端目录，得到: %q", got)
	}
	if !strings.Contains(got, "不是用户指定的工作区") {
		t.Fatalf("披露进程目录时必须声明它不是用户工作区，得到: %q", got)
	}
}

// TestComposeSystemBaseBound 已绑定工作区：声明真实目录并给出相对路径根口径。
func TestComposeSystemBaseBound(t *testing.T) {
	got := ComposeSystemBase("你是编码助手。", `D:\WorkSpace\Project\harness_mini`, "")
	if !strings.Contains(got, "已绑定工作区") || !strings.Contains(got, "harness_mini") {
		t.Fatalf("已绑定会话必须声明真实目录，得到: %q", got)
	}
	if strings.Contains(got, "未绑定工作区") {
		t.Fatalf("已绑定会话不得出现未绑定声明，得到: %q", got)
	}
}

// TestComposeSystemBaseTrim 空基础提示 + 空白工作区：只输出工作区段落，不留前导空行。
func TestComposeSystemBaseTrim(t *testing.T) {
	got := ComposeSystemBase("   ", "   ", "")
	if strings.HasPrefix(got, "\n") {
		t.Fatalf("不应以空行开头，得到: %q", got)
	}
	if !strings.Contains(got, "未绑定工作区") {
		t.Fatalf("空白工作区按未绑定处理，得到: %q", got)
	}
}
