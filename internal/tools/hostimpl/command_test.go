package hostimpl

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"harness-go/pkg/toolapi"
)

// newRunner 构造一个以 t.TempDir 为根的执行器。
func newRunner(t *testing.T) toolapi.CommandRunner {
	t.Helper()
	return NewCommandRunner(CommandOptions{Root: t.TempDir()})
}

// shellCmd 返回当前平台下「执行一段脚本」的 cmd/args。
func shellCmd(script string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell", []string{"-NoProfile", "-Command", script}
	}
	return "sh", []string{"-c", script}
}

// TestCommandSuccess 正常命令：退出码 0，输出完整。
func TestCommandSuccess(t *testing.T) {
	cmd, args := shellCmd("echo hello")
	res, err := newRunner(t).Run(context.Background(), toolapi.CommandRequest{
		Cmd: cmd, Args: args, Timeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("退出码应为 0，得到 %d", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "hello") {
		t.Errorf("stdout 应包含 hello，得到 %q", res.Stdout)
	}
	if res.TimedOut || res.Cancelled {
		t.Error("正常结束不应标记超时或取消")
	}
}

// TestCommandNonZeroExit 非零退出码是**结果**不是执行器故障。
//
// 契约：必须返回 nil error + 真实 ExitCode，让模型读 stderr 自我纠正。
// 若改成返回 error，模型就失去了修正机会（只会看到"工具坏了"）。
func TestCommandNonZeroExit(t *testing.T) {
	cmd, args := shellCmd("Write-Output 'bad'; exit 3")
	if runtime.GOOS != "windows" {
		cmd, args = shellCmd("echo bad; exit 3")
	}
	res, err := newRunner(t).Run(context.Background(), toolapi.CommandRequest{
		Cmd: cmd, Args: args, Timeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("非零退出不应是执行器错误: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("退出码应非 0，得到 %d（stdout=%q stderr=%q）", res.ExitCode, res.Stdout, res.Stderr)
	}
	if res.TimedOut || res.Cancelled {
		t.Error("非零退出不应被误标为超时或取消")
	}
}

// TestCommandTimeout 超时必须标记 TimedOut（而不是落入通用错误分支）。
//
// 回归背景：旧实现 DefaultCommandTimeout=60s，但实测 84.6s 才结束，
// 且标记成 exit=1 / context canceled —— 说明超时判定本身没生效。
func TestCommandTimeout(t *testing.T) {
	cmd, args := shellCmd("Start-Sleep -Seconds 30")
	if runtime.GOOS != "windows" {
		cmd, args = shellCmd("sleep 30")
	}
	start := time.Now()
	res, err := newRunner(t).Run(context.Background(), toolapi.CommandRequest{
		Cmd: cmd, Args: args, Timeout: 1 * time.Second,
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("超时应是结果而非执行器错误: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("必须标记 TimedOut，得到 TimedOut=%v Cancelled=%v（stderr=%q）",
			res.TimedOut, res.Cancelled, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "超时") {
		t.Errorf("提示语应说明超时，得到 %q", res.Stderr)
	}
	// 超时后必须迅速收敛：留 10s 余量容忍 Windows 进程回收开销。
	if elapsed > 11*time.Second {
		t.Errorf("超时后收敛过慢（%s），进程树可能未被有效终止", elapsed)
	}
}

// TestCommandCancelDistinctFromTimeout 取消必须与超时严格区分。
//
// 这是本次修复的核心断言之一：用户点停止（context.Canceled）绝不能被
// 报成 timeout，更不能变成 "启动命令失败: context canceled" 这种
// 模型无法解读的通用错误。
func TestCommandCancelDistinctFromTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd, args := shellCmd("Start-Sleep -Seconds 30")
	if runtime.GOOS != "windows" {
		cmd, args = shellCmd("sleep 30")
	}

	// 200ms 后取消，模拟用户点停止。
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	res, err := newRunner(t).Run(ctx, toolapi.CommandRequest{
		Cmd: cmd, Args: args, Timeout: 60 * time.Second,
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("取消应是结果而非执行器错误（旧实现正是在这里返回 context canceled）: %v", err)
	}
	if !res.Cancelled {
		t.Errorf("必须标记 Cancelled，得到 Cancelled=%v TimedOut=%v（stderr=%q）",
			res.Cancelled, res.TimedOut, res.Stderr)
	}
	if res.TimedOut {
		t.Error("取消不得被误标为超时")
	}
	if !strings.Contains(res.Stderr, "取消") {
		t.Errorf("提示语应说明已取消，得到 %q", res.Stderr)
	}
	// 关键：取消后必须立刻收敛，不能等到 timeout 才结束。
	// 这一条直接对应事故里的「终态后 53.7 秒仍有消息入库」。
	if elapsed > 10*time.Second {
		t.Errorf("取消后收敛过慢（%s），说明进程树未被有效终止（孙进程仍在跑）", elapsed)
	}
}

// TestProcessTreeKillIdempotent Kill 必须幂等，重复调用不 panic。
func TestProcessTreeKillIdempotent(t *testing.T) {
	tree := attachProcessTree(0) // PID 0 是安全哨兵，killTree 内部会直接返回
	tree.Kill()
	tree.Kill()
	tree.Release()
	tree.Release()
	if tree.PID() != 0 {
		t.Errorf("PID 应为 0，得到 %d", tree.PID())
	}
}

// TestNilProcessTreeSafe nil 守卫不得 panic。
func TestNilProcessTreeSafe(t *testing.T) {
	var tree *ProcessTree
	tree.Kill()
	tree.Release()
	if tree.PID() != 0 {
		t.Error("nil 的 PID 应为 0")
	}
	quitProcessTree(nil, true)
}

// TestCommandEmptyCmdRejected 空命令必须拒绝，不得启动空进程。
func TestCommandEmptyCmdRejected(t *testing.T) {
	_, err := newRunner(t).Run(context.Background(), toolapi.CommandRequest{Cmd: "  "})
	if err == nil {
		t.Fatal("空命令必须报错")
	}
}
