package todo

import (
	"context"
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

// TestTodoRoundTrip 写读往返 + 状态图标。
func TestTodoRoundTrip(t *testing.T) {
	h := plugintest.New(t)
	ctx := context.Background()

	// 读：空清单
	res, _ := invokeRead(ctx, h, &toolapi.Call{})
	if !strings.Contains(res.Content, "无任务清单") {
		t.Fatalf("空清单口径错误: %q", res.Content)
	}
	// 写
	res, err := invokeWrite(ctx, h, &toolapi.Call{Args: []byte(
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
	// 再读
	res, _ = invokeRead(ctx, h, &toolapi.Call{})
	if !strings.Contains(res.Content, "3. ○ 收尾") {
		t.Fatalf("回读错误: %q", res.Content)
	}
}

// TestTodoValidation 非法状态拒绝。
func TestTodoValidation(t *testing.T) {
	h := plugintest.New(t)
	res, _ := invokeWrite(context.Background(), h, &toolapi.Call{Args: []byte(
		`{"items":[{"content":"x","status":"doing"}]}`)})
	if !res.IsError || !strings.Contains(res.Content, "状态非法") {
		t.Fatalf("非法状态必须拒绝: %q", res.Content)
	}
	// 空内容
	res, _ = invokeWrite(context.Background(), h, &toolapi.Call{Args: []byte(
		`{"items":[{"content":"  ","status":"done"}]}`)})
	if !res.IsError {
		t.Fatal("空内容必须拒绝")
	}
}
