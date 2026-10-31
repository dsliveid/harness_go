package decision

import (
	"context"
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

func TestDecisionRecordList(t *testing.T) {
	h := plugintest.New(t)
	ctx := context.Background()
	if _, err := invokeRecord(ctx, h, &toolapi.Call{Args: []byte(
		`{"title":"存储选型","choice":"SQLite 分库","reason":"单机一等公民"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := invokeRecord(ctx, h, &toolapi.Call{Args: []byte(
		`{"title":"前端框架","choice":"React 18"}`)}); err != nil {
		t.Fatal(err)
	}
	res, _ := invokeList(ctx, h, &toolapi.Call{Args: []byte(`{}`)})
	c := res.Content
	if !strings.Contains(c, "共 2 条") || !strings.Contains(c, "前端框架 → React 18") {
		t.Fatalf("列表错误: %q", c)
	}
	// 最近在前
	if strings.Index(c, "前端框架") > strings.Index(c, "存储选型") {
		t.Fatalf("应最近在前: %q", c)
	}
	// 空字段拒绝
	res, _ = invokeRecord(ctx, h, &toolapi.Call{Args: []byte(`{"title":"","choice":"x"}`)})
	if !res.IsError {
		t.Fatal("空 title 必须拒绝")
	}
}
