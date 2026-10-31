package command

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

// TestRunCommandReal 真实执行（走 hostimpl.CommandRunner 的同语义桩实现）。
func TestRunCommandReal(t *testing.T) {
	h := plugintest.New(t)
	var chunks []string
	h.ProgressCh = make(chan string, 64)
	h.ExecFn = func(ctx context.Context, req toolapi.CommandRequest) (toolapi.CommandResult, error) {
		if req.Cmd == "failing" {
			return toolapi.CommandResult{ExitCode: 2, Stderr: "boom\n"}, nil
		}
		return toolapi.CommandResult{ExitCode: 0, Stdout: "hello out\n"}, nil
	}
	_ = chunks

	// ① 成功路径
	res, err := invokeRunCommand(context.Background(), h, &toolapi.Call{
		Args: []byte(`{"cmd":"echo","args":["hi"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Content, "exit=0") || !strings.Contains(res.Content, "hello out") {
		t.Fatalf("结果错误: %q", res.Content)
	}

	// ② 非零退出码 → IsError（stderr 回传模型自我纠正）
	res, _ = invokeRunCommand(context.Background(), h, &toolapi.Call{Args: []byte(`{"cmd":"failing"}`)})
	if !res.IsError || !strings.Contains(res.Content, "boom") {
		t.Fatalf("失败结果错误: %q", res.Content)
	}
}

// TestRunCommandTimeoutMarked 超时标记回传（hostimpl 语义）。
func TestRunCommandTimeoutMarked(t *testing.T) {
	h := plugintest.New(t)
	h.ExecFn = func(ctx context.Context, req toolapi.CommandRequest) (toolapi.CommandResult, error) {
		return toolapi.CommandResult{TimedOut: true, Stderr: "[超时]"}, nil
	}
	res, _ := invokeRunCommand(context.Background(), h, &toolapi.Call{Args: []byte(`{"cmd":"slow"}`)})
	if !strings.Contains(res.Content, "timeout") {
		t.Fatalf("超时应标记: %q", res.Content)
	}
}

// TestRunCommandProgressWiring progress 回调注入（Host.Events 存在时）。
func TestRunCommandProgressWiring(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Log("Windows 下 progress 节流由 hostimpl 集成测试覆盖")
	}
	h := plugintest.New(t)
	h.ProgressCh = make(chan string, 16)
	h.ExecFn = func(ctx context.Context, req toolapi.CommandRequest) (toolapi.CommandResult, error) {
		if req.Progress != nil {
			req.Progress("step1\n")
			req.Progress("step2\n")
		}
		return toolapi.CommandResult{ExitCode: 0}, nil
	}
	res, _ := invokeRunCommand(context.Background(), h, &toolapi.Call{
		RunID: "run-1", ToolEventID: "te-1", Args: []byte(`{"cmd":"build"}`),
	})
	if res.IsError {
		t.Fatalf("结果错误: %q", res.Content)
	}
	close(h.ProgressCh)
	got := ""
	for chunk := range h.ProgressCh {
		got += chunk
	}
	if !strings.Contains(got, "step1") || !strings.Contains(got, "step2") {
		t.Fatalf("progress 未推送: %q", got)
	}
}

// TestRunCommandEmptyCmd 空命令拒绝。
func TestRunCommandEmptyCmd(t *testing.T) {
	h := plugintest.New(t)
	res, _ := invokeRunCommand(context.Background(), h, &toolapi.Call{Args: []byte(`{"cmd":"  "}`)})
	if !res.IsError {
		t.Fatal("空命令必须拒绝")
	}
}
