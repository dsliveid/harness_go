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

// invoke 单工具入口：传 items 即覆写，不传即读取。
//
// # 为什么合并成一个工具（而不是 todo_write / todo_read 两个）
//
// 提示词（SOP）里要求模型调用的是 `todo`。拆成两个名字会让"文本说的"
// 与"实际注册的"永久对不上 —— 模型按提示词调用会拿到「工具未注册」。
//
// 合并的语义依据：读与写操作**同一个对象**（同一份清单文件），
// 用一个工具 + 可选参数表达比两个独立名字更贴近意图，也少占一个工具的
// 注入预算。缺省行为选"读取"：读取是幂等且无副作用的，误调用代价最低。
//
// 注意 risk 声明为 write：合并后该工具**具备**写入能力，按最严级别声明，
// 宁可多一次审批，也不让一个有写能力的工具以 readonly 身份绕过审批闸门。
func invoke(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Items []Item `json:"items"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("todo: %v", err), nil
	}
	// 未传 items（nil）→ 读取。注意与"传了空数组"区分：
	// 空数组是模型显式要求清空，应当拒绝（清单不允许为空，否则失去跟踪意义）。
	if args.Items == nil {
		return readTodo(ctx, host)
	}
	return writeTodo(ctx, host, args.Items)
}

// writeTodo 整体覆写清单。
func writeTodo(ctx context.Context, host toolapi.Host, items []Item) (*toolapi.Result, error) {
	if len(items) == 0 {
		return pluginkit.ErrorResult("todo: items 不能为空（清空清单请逐项置 done 或直接说明）"), nil
	}
	for i, it := range items {
		if strings.TrimSpace(it.Content) == "" {
			return pluginkit.ErrorResult("todo: 第 %d 项 content 为空", i+1), nil
		}
		if !validStatus(it.Status) {
			return pluginkit.ErrorResult("todo: 第 %d 项状态非法 %q（可选 pending|in_progress|done）", i+1, it.Status), nil
		}
	}
	raw, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return pluginkit.ErrorResult("todo: 序列化失败: %v", err), nil
	}
	if err := host.Workspace().WriteFile(ctx, StorePath, raw, 0o644); err != nil {
		return pluginkit.ErrorResult("todo: 写入失败: %v", err), nil
	}
	return pluginkit.TextResult("%s", render(items)), nil
}

// readTodo 读取清单。
func readTodo(ctx context.Context, host toolapi.Host) (*toolapi.Result, error) {
	data, err := host.Workspace().ReadFile(ctx, StorePath)
	if err != nil {
		return pluginkit.TextResult("当前无任务清单"), nil
	}
	var items []Item
	if err := json.Unmarshal(data, &items); err != nil {
		return pluginkit.ErrorResult("todo: 清单文件损坏: %v", err), nil
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
