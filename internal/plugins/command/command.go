package command

import (
	"context"
	"fmt"
	"strings"
	"time"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

// invokeRunCommand 执行命令：审批通过后由引擎进入本工具，
// 增量输出经 tool.progress 推给前端，终态（stdout/stderr/退出码）作为工具结果。
func invokeRunCommand(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Cmd      string   `json:"cmd"`
		Args     []string `json:"args"`
		Dir      string   `json:"dir"`
		TimeoutS int      `json:"timeout_s"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("run_command: %v", err), nil
	}
	if strings.TrimSpace(args.Cmd) == "" {
		return pluginkit.ErrorResult("run_command: cmd 不能为空"), nil
	}
	req := toolapi.CommandRequest{
		Cmd:         args.Cmd,
		Args:        args.Args,
		Dir:         args.Dir,
		ToolEventID: call.ToolEventID,
	}
	if args.TimeoutS > 0 {
		req.Timeout = time.Duration(args.TimeoutS) * time.Second
	}
	if host.Events() != nil && call.RunID != "" {
		// 实时输出（需求 §10-4）：progress 事件带 tool_event_id 供前端定位工具卡片
		req.Progress = func(chunk string) {
			_ = host.Events().Publish(ctx, call.RunID, "tool.progress", map[string]any{
				"tool_event_id": call.ToolEventID,
				"chunk":         chunk,
			})
		}
	}
	res, err := host.Exec().Run(ctx, req)
	if err != nil {
		return pluginkit.ErrorResult("run_command: 执行失败: %v", err), nil
	}
	var b strings.Builder
	status := fmt.Sprintf("exit=%d", res.ExitCode)
	if res.TimedOut {
		status = "timeout"
	}
	fmt.Fprintf(&b, "命令 %s %s\n%s\n", args.Cmd, strings.Join(args.Args, " "), status)
	if out := strings.TrimRight(res.Stdout, "\n"); out != "" {
		b.WriteString("\n[stdout]\n" + out + "\n")
	}
	if e := strings.TrimRight(res.Stderr, "\n"); e != "" {
		b.WriteString("\n[stderr]\n" + e + "\n")
	}
	if res.ExitCode != 0 && !res.TimedOut {
		return &toolapi.Result{Content: b.String(), IsError: true}, nil
	}
	return pluginkit.TextResult("%s", b.String()), nil
}
