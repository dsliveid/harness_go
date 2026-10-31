package todo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

// Item 清单项。
type Item struct {
	Content string `json:"content"`
	Status  string `json:"status"` // pending | in_progress | done
}

func validStatus(s string) bool {
	switch s {
	case "pending", "in_progress", "done":
		return true
	}
	return false
}

// invokeWrite 整体覆写清单。
func invokeWrite(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Items []Item `json:"items"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("todo_write: %v", err), nil
	}
	if len(args.Items) == 0 {
		return pluginkit.ErrorResult("todo_write: items 不能为空（清空清单请逐项置 done 或直接说明）"), nil
	}
	for i, it := range args.Items {
		if strings.TrimSpace(it.Content) == "" {
			return pluginkit.ErrorResult("todo_write: 第 %d 项 content 为空", i+1), nil
		}
		if !validStatus(it.Status) {
			return pluginkit.ErrorResult("todo_write: 第 %d 项状态非法 %q（可选 pending|in_progress|done）", i+1, it.Status), nil
		}
	}
	raw, err := json.MarshalIndent(args.Items, "", "  ")
	if err != nil {
		return pluginkit.ErrorResult("todo_write: 序列化失败: %v", err), nil
	}
	if err := host.Workspace().WriteFile(ctx, StorePath, raw, 0o644); err != nil {
		return pluginkit.ErrorResult("todo_write: 写入失败: %v", err), nil
	}
	return pluginkit.TextResult("%s", render(args.Items)), nil
}

// invokeRead 读取清单。
func invokeRead(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	data, err := host.Workspace().ReadFile(ctx, StorePath)
	if err != nil {
		return pluginkit.TextResult("当前无任务清单"), nil
	}
	var items []Item
	if err := json.Unmarshal(data, &items); err != nil {
		return pluginkit.ErrorResult("todo_read: 清单文件损坏: %v", err), nil
	}
	if len(items) == 0 {
		return pluginkit.TextResult("当前任务清单为空"), nil
	}
	return pluginkit.TextResult("%s", render(items)), nil
}

func render(items []Item) string {
	var b strings.Builder
	b.WriteString("任务清单：\n")
	icon := map[string]string{"pending": "○", "in_progress": "◐", "done": "●"}
	for i, it := range items {
		fmt.Fprintf(&b, "%d. %s %s\n", i+1, icon[it.Status], it.Content)
	}
	return strings.TrimRight(b.String(), "\n")
}
