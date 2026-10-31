package plan

import (
	"context"
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

func TestPlanRoundTrip(t *testing.T) {
	h := plugintest.New(t)
	ctx := context.Background()
	res, _ := invokeGet(ctx, h, &toolapi.Call{})
	if !strings.Contains(res.Content, "无执行计划") {
		t.Fatalf("空计划口径错误: %q", res.Content)
	}
	res, err := invokeSet(ctx, h, &toolapi.Call{Args: []byte(
		`{"title":"重构检索","steps":[{"title":"索引层"},{"title":"选择器","done":true}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "重构检索（1/2 完成）") || !strings.Contains(res.Content, "● 选择器") {
		t.Fatalf("渲染错误: %q", res.Content)
	}
	res, _ = invokeGet(ctx, h, &toolapi.Call{})
	if !strings.Contains(res.Content, "重构检索") {
		t.Fatalf("回读错误: %q", res.Content)
	}
}
