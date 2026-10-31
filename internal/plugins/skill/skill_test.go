package skill

import (
	"context"
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

func TestSkillSaveListRead(t *testing.T) {
	h := plugintest.New(t)
	ctx := context.Background()
	res, _ := invokeList(ctx, h, &toolapi.Call{})
	if !strings.Contains(res.Content, "技能库为空") {
		t.Fatalf("空库口径错误: %q", res.Content)
	}
	if _, err := invokeSave(ctx, h, &toolapi.Call{Args: []byte(
		`{"name":"release-check","description":"发布前检查清单（每次发版后）","body":"1. 跑测试\n2. 打标签"}`)}); err != nil {
		t.Fatal(err)
	}
	res, _ = invokeList(ctx, h, &toolapi.Call{})
	if !strings.Contains(res.Content, "release-check：发布前检查清单") {
		t.Fatalf("列表错误: %q", res.Content)
	}
	res, _ = invokeRead(ctx, h, &toolapi.Call{Args: []byte(`{"name":"release-check"}`)})
	if !strings.Contains(res.Content, "1. 跑测试") || !strings.Contains(res.Content, "发布前检查") {
		t.Fatalf("读取错误: %q", res.Content)
	}
	// 非法名拒绝
	res, _ = invokeRead(ctx, h, &toolapi.Call{Args: []byte(`{"name":"../evil"}`)})
	if !res.IsError {
		t.Fatal("非法技能名必须拒绝")
	}
}
