package collaborator

import (
	"context"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

func invokeAsk(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Role     string `json:"role"`
		Question string `json:"question"`
		ModelRef string `json:"model_ref"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("ask_collaborator: %v", err), nil
	}
	if host.Spawn() == nil {
		return pluginkit.ErrorResult("ask_collaborator: 嵌套 Run 能力未接线"), nil
	}
	child, err := host.Spawn().Spawn(ctx, toolapi.SpawnRequest{
		ParentRunID: call.RunID,
		SessionID:   call.SessionID,
		AgentRole:   args.Role,
		Input:       args.Question,
		ModelRef:    args.ModelRef,
	})
	if err != nil {
		return pluginkit.ErrorResult("ask_collaborator: %v", err), nil
	}
	res, err := child.Wait(ctx)
	if err != nil {
		return pluginkit.ErrorResult("ask_collaborator: 等待回复失败: %v", err), nil
	}
	if res.Status != toolapi.ChildCompleted {
		detail := res.Error
		if detail == "" {
			detail = string(res.Status)
		}
		return &toolapi.Result{Content: "协作者未成功回复（" + detail + "）", IsError: true,
			Meta: map[string]any{"child_run_id": res.RunID, "child_status": string(res.Status)}}, nil
	}
	return pluginkit.TextResult("[%s 的意见]\n%s", args.Role, res.Output), nil
}
