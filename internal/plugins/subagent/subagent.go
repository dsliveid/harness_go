package subagent

import (
	"context"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

func invokeSpawn(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Task     string `json:"task"`
		ModelRef string `json:"model_ref"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("spawn_subagent: %v", err), nil
	}
	if host.Spawn() == nil {
		return pluginkit.ErrorResult("spawn_subagent: 嵌套 Run 能力未接线"), nil
	}
	child, err := host.Spawn().Spawn(ctx, toolapi.SpawnRequest{
		ParentRunID: call.RunID,
		SessionID:   call.SessionID,
		AgentRole:   "subagent",
		Input:       args.Task,
		ModelRef:    args.ModelRef,
	})
	if err != nil {
		return pluginkit.ErrorResult("spawn_subagent: %v", err), nil
	}
	res, err := child.Wait(ctx)
	if err != nil {
		return pluginkit.ErrorResult("spawn_subagent: 等待子 Run 失败: %v", err), nil
	}
	if res.Status != toolapi.ChildCompleted {
		detail := res.Error
		if detail == "" {
			detail = string(res.Status)
		}
		return &toolapi.Result{Content: "子代理未成功完成（" + detail + "）", IsError: true,
			Meta: map[string]any{"child_run_id": res.RunID, "child_status": string(res.Status)}}, nil
	}
	return pluginkit.TextResult("子代理完成（run=%s）：\n%s", res.RunID, res.Output), nil
}
