package todo

import (
	"context"
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

// TestTodoRoundTrip 写读往返 + 状态图标（单工具：传 items 写，不传则读）。
func TestTodoRoundTrip(t *testing.T) {
	h := plugintest.New(t)
	ctx := context.Background()

	// 读：空清单（不传 items）
	res, _ := invoke(ctx, h, &toolapi.Call{})
	if !strings.Contains(res.Content, "无任务清单") {
		t.Fatalf("空清单口径错误: %q", res.Content)
	}
	// 写（传 items）
	res, err := invoke(ctx, h, &toolapi.Call{Args: []byte(
		`{"items":[{"content":"实现 fs 插件","status":"done"},{"content":"写测试","status":"in_progress"},{"content":"收尾","status":"pending"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Content
	if !strings.Contains(c, "● 实现 fs 插件") || !strings.Contains(c, "◐ 写测试") || !strings.Contains(c, "○ 收尾") {
		t.Fatalf("渲染错误: %q", c)
	}
	// 落盘校验
	data, err := h.ReadFile(ctx, StorePath)
	if err != nil || !strings.Contains(string(data), "in_progress") {
		t.Fatalf("落盘错误: %v", err)
	}
	// 再读（不传 items）
	res, _ = invoke(ctx, h, &toolapi.Call{})
	if !strings.Contains(res.Content, "3. ○ 收尾") {
		t.Fatalf("回读错误: %q", res.Content)
	}
}

// TestTodoValidation 非法状态拒绝。
//
// 注（2026-11 变更）：`doing` / `completed` 等**同义词已被归一**，不再算非法
// （模型稳定倾向使用它们，报错只会换来一次无意义的往返）。
// 因此这条断言改用真正无法映射的词（`archived`）。
func TestTodoValidation(t *testing.T) {
	h := plugintest.New(t)
	ctx := context.Background()
	res, _ := invoke(ctx, h, &toolapi.Call{Args: []byte(
		`{"items":[{"content":"x","status":"archived"}]}`)})
	if !res.IsError || !strings.Contains(res.Content, "状态非法") {
		t.Fatalf("非法状态必须拒绝: %q", res.Content)
	}
	// 空内容
	res, _ = invoke(ctx, h, &toolapi.Call{Args: []byte(
		`{"items":[{"content":"  ","status":"done"}]}`)})
	if !res.IsError {
		t.Fatal("空内容必须拒绝")
	}
}

// TestTodoStatusSynonymNormalized 同义状态词必须被归一而不是报错。
//
// 事故会话 122559a3 的实测：模型写 `completed` 被直接拒绝，
// 白烧一步且要靠"再猜一次"纠正。归一化的契约是「规范词表不变，只宽容同义输入」。
func TestTodoStatusSynonymNormalized(t *testing.T) {
	h := plugintest.New(t)
	ctx := context.Background()
	res, _ := invoke(ctx, h, &toolapi.Call{Args: []byte(
		`{"items":[{"content":"a","status":"completed"},{"content":"b","status":"doing"}]}`)})
	if res.IsError {
		t.Fatalf("同义状态词不应报错: %q", res.Content)
	}
	// 输出必须已归一为规范词（● = done，◐ = in_progress）。
	out, _ := invoke(ctx, h, &toolapi.Call{})
	if !strings.Contains(out.Content, "1. ● a") {
		t.Fatalf("completed 应归一为 done，实际: %q", out.Content)
	}
	if !strings.Contains(out.Content, "2. ◐ b") {
		t.Fatalf("doing 应归一为 in_progress，实际: %q", out.Content)
	}
}

// TestTodoEmptyItemsRejected 显式传空数组必须拒绝（与"不传 items 表示读取"区分）。
//
// 这是合并成单工具后最容易搞错的一处：JSON 里 `[]` 与缺省在 Go 解码后
// 分别是「非 nil 空切片」与「nil 切片」。若两者都当成读取，模型想"清空清单"
// 时会静默变成"读了一遍"，行为与意图不符且无从察觉。
func TestTodoEmptyItemsRejected(t *testing.T) {
	h := plugintest.New(t)
	res, _ := invoke(context.Background(), h, &toolapi.Call{Args: []byte(`{"items":[]}`)})
	if !res.IsError || !strings.Contains(res.Content, "items 不能为空") {
		t.Fatalf("显式空数组必须拒绝（不能当成读取）: isError=%v %q", res.IsError, res.Content)
	}
}

// TestManifestDeclaresSingleTool 清单必须只声明一个 todo 工具。
//
// 提示词里教模型调用的名字就是 `todo`；manifest 若仍是 todo_write/todo_read，
// 模型按提示词调用会拿到「工具未注册」的软失败（本包正是为此归一）。
func TestManifestDeclaresSingleTool(t *testing.T) {
	tools := Manifest().Tools
	if len(tools) != 1 || tools[0].Name != "todo" {
		names := make([]string, 0, len(tools))
		for _, s := range tools {
			names = append(names, s.Name)
		}
		t.Fatalf("manifest 必须只声明 todo 一个工具，实际: %v", names)
	}
}
