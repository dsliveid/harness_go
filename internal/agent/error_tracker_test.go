package agent

// 工具失败/重复调用闸门的回归测试。
//
// 对应 2026-11 事故会话 9ee34e00 的真实形态：同类 go build 连跑 3 次、
// 累计 271 秒，模型完全没有被告知「你已经失败过、参数还一模一样」。

import (
	"strings"
	"testing"

	"harness-go/internal/model"
)

// TestDuplicateFailingCallBlocked 完全重复的调用必须在第 3 次起被拦下。
//
// 这是 P1-7 的核心断言：拦截不能只作用于失败路径（成功的重复调用同样是纯浪费，
// 结果已在上下文里）。
func TestDuplicateFailingCallBlocked(t *testing.T) {
	tr := newToolErrorTracker()
	args := []byte(`{"cmd":"go","args":["build","./cmd/x"]}`)

	// 前两次放行（允许「重跑一次确认非偶发」这一合法用法）。
	for i := 1; i <= maxIdenticalCalls; i++ {
		if allowed, notice := tr.allow("run_command", args); !allowed {
			t.Fatalf("第 %d 次调用不应被拦，notice=%q", i, notice)
		}
	}
	allowed, notice := tr.allow("run_command", args)
	if allowed {
		t.Fatal("完全相同的参数第三次调用必须被拦下（否则复现 271 秒无效重试）")
	}
	if !strings.Contains(notice, "已被拦截") {
		t.Fatalf("拦截必须把原因回灌给模型，实际 notice=%q", notice)
	}
	// 拦截文案必须给出可执行的出路（不是一句"不许调用"）。
	for _, want := range []string{"改变参数范围", "换用其它工具"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("拦截文案缺少出路提示 %q：%s", want, notice)
		}
	}
}

// TestDuplicateDetectionIgnoresWhitespace 参数空白差异不得绕过重复检测。
//
// 模型很容易靠改缩进/空格"换个说法"，那在语义上是同一个调用。
func TestDuplicateDetectionIgnoresWhitespace(t *testing.T) {
	tr := newToolErrorTracker()
	a := []byte(`{"path":"a.go","limit":10}`)
	b := []byte("{\n  \"path\": \"a.go\",\n  \"limit\": 10\n}")

	for i := 0; i < maxIdenticalCalls; i++ {
		tr.allow("read_file", a)
	}
	if allowed, _ := tr.allow("read_file", b); allowed {
		t.Fatal("仅空白不同的参数必须视为同一调用")
	}
}

// TestDifferentArgsNotBlocked 参数确实不同时必须放行（闸门不能误伤正常重试）。
func TestDifferentArgsNotBlocked(t *testing.T) {
	tr := newToolErrorTracker()
	tr.allow("read_file", []byte(`{"path":"a.go"}`))
	tr.allow("read_file", []byte(`{"path":"a.go"}`))
	if allowed, notice := tr.allow("read_file", []byte(`{"path":"b.go"}`)); !allowed {
		t.Fatalf("参数不同应放行，notice=%q", notice)
	}
}

// TestConsecutiveFailureWarningInjected 连续失败超限后必须注入中文自纠提示。
func TestConsecutiveFailureWarningInjected(t *testing.T) {
	tr := newToolErrorTracker()
	// 每次都用**不同参数**，避免撞上重复调用闸门（只验证失败计数分支）。
	overLimit := false
	var n int
	for i := 0; i <= maxConsecutiveToolFailures; i++ {
		tr.allow("edit_file", []byte(`{"path":"a.go","n":`+itoa(i)+`}`))
		overLimit, n = tr.recordFailure("edit_file")
	}
	if !overLimit {
		t.Fatalf("连续失败 %d 次后必须超限", maxConsecutiveToolFailures+1)
	}
	warning := toolFailureWarning("edit_file", n)
	// 核心口径：必须明确"先读原文再改"，且明令禁止盲目重试（对齐老版本文案）。
	for _, want := range []string{"read_file", "严禁不作分析直接重复调用相同参数"} {
		if !strings.Contains(warning, want) {
			t.Fatalf("自纠提示缺少 %q：%s", want, warning)
		}
	}
}

// TestSuccessResetsFailureCount 成功必须清零失败计数（偶发失败不该累积成"坏工具"）。
func TestSuccessResetsFailureCount(t *testing.T) {
	tr := newToolErrorTracker()
	tr.recordFailure("run_command")
	tr.recordFailure("run_command")
	tr.recordSuccess("run_command")
	if _, n := tr.recordFailure("run_command"); n != 1 {
		t.Fatalf("成功应清零计数，实际从 %d 重新开始", n)
	}
}

// toolMsgFor 构造测试用的 tool 消息。
func toolMsgFor(content string) *model.Message {
	return &model.Message{Role: model.RoleTool, Content: content}
}

// TestIsToolFailureRecognizesExecError 失败判定必须认得 execTool 的各类失败文案。
func TestIsToolFailureRecognizesExecError(t *testing.T) {
	cases := map[string]bool{
		"工具执行失败: exit=1":        true,
		"denied: 风险过高":          true,
		"审批登记失败: x":             true,
		"命令 go build\nexit=0\n": false,
		"":                      false,
	}
	for content, want := range cases {
		got := isToolFailure(toolMsgFor(content))
		if got != want {
			t.Fatalf("isToolFailure(%q) = %v，期望 %v", content, got, want)
		}
	}
	if isToolFailure(nil) {
		t.Fatal("nil 消息不得判为失败")
	}
}
