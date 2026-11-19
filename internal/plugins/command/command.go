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
	// 状态行必须三分流：超时 / 取消 / 正常退出码。
	// 模型据此决定「换策略重试」还是「放弃这条路径」——旧实现只区分
	// timeout 与 exit，取消被混成 exit=-1 或直接变成执行器错误，
	// 模型会误以为命令跑完了只是失败了，进而反复重跑已被用户中止的操作。
	status := fmt.Sprintf("exit=%d", res.ExitCode)
	switch {
	case res.TimedOut:
		status = "timeout"
	case res.Cancelled:
		status = "cancelled"
	}
	fmt.Fprintf(&b, "命令 %s %s\n%s\n", args.Cmd, strings.Join(args.Args, " "), status)
	if out := strings.TrimRight(res.Stdout, "\n"); out != "" {
		b.WriteString("\n[stdout]\n" + out + "\n")
	}
	if e := strings.TrimRight(res.Stderr, "\n"); e != "" {
		b.WriteString("\n[stderr]\n" + e + "\n")
	}
	// 取消不算错误：它是用户意图，不该以 IsError 形式回灌模型
	// （IsError 会触发模型的"工具坏了"解读，而实际是用户主动停止）。
	if res.Cancelled {
		return pluginkit.TextResult("%s", b.String()), nil
	}
	if res.ExitCode != 0 && !res.TimedOut {
		return &toolapi.Result{Content: b.String(), IsError: true}, nil
	}
	return pluginkit.TextResult("%s", b.String()), nil
}
