package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

// Step 计划步骤。
type Step struct {
	Title string `json:"title"`
	Done  bool   `json:"done"`
}

// Plan 计划文档。
type Plan struct {
	Title string `json:"title"`
	Steps []Step `json:"steps"`
}

func invokeSet(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args Plan
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("plan_set: %v", err), nil
	}
	if strings.TrimSpace(args.Title) == "" {
		return pluginkit.ErrorResult("plan_set: title 不能为空"), nil
	}
	if len(args.Steps) == 0 {
		return pluginkit.ErrorResult("plan_set: steps 不能为空"), nil
	}
	raw, err := json.MarshalIndent(args, "", "  ")
	if err != nil {
		return pluginkit.ErrorResult("plan_set: 序列化失败: %v", err), nil
	}
	if err := host.Workspace().WriteFile(ctx, StorePath, raw, 0o644); err != nil {
		return pluginkit.ErrorResult("plan_set: 写入失败: %v", err), nil
	}
	return pluginkit.TextResult("%s", render(args)), nil
}

func invokeGet(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	data, err := host.Workspace().ReadFile(ctx, StorePath)
	if err != nil {
		return pluginkit.TextResult("当前无执行计划"), nil
	}
	var p Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return pluginkit.ErrorResult("plan_get: 计划文件损坏: %v", err), nil
	}
	return pluginkit.TextResult("%s", render(p)), nil
}

func render(p Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "计划：%s（%d/%d 完成）\n", p.Title, countDone(p.Steps), len(p.Steps))
	for i, s := range p.Steps {
		mark := "○"
		if s.Done {
			mark = "●"
		}
		fmt.Fprintf(&b, "%d. %s %s\n", i+1, mark, s.Title)
	}
	return strings.TrimRight(b.String(), "\n")
}

func countDone(steps []Step) int {
	n := 0
	for _, s := range steps {
		if s.Done {
			n++
		}
	}
	return n
}
