package growth

import (
	"context"
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

func TestGrowthRecordListApply(t *testing.T) {
	h := plugintest.New(t)
	ctx := context.Background()
	if _, err := invokeRecord(ctx, h, &toolapi.Call{Args: []byte(
		`{"lesson":"Windows 路径分隔符用 filepath","context":"文件操作"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := invokeRecord(ctx, h, &toolapi.Call{Args: []byte(
		`{"lesson":"先读后写，diff 可回滚"}`)}); err != nil {
		t.Fatal(err)
	}
	res, _ := invokeList(ctx, h, &toolapi.Call{Args: []byte(`{}`)})
	if !strings.Contains(res.Content, "共 2 条，未应用 2") {
		t.Fatalf("列表错误: %q", res.Content)
	}
	if _, err := invokeApply(ctx, h, &toolapi.Call{Args: []byte(`{"id":1}`)}); err != nil {
		t.Fatal(err)
	}
	res, _ = invokeList(ctx, h, &toolapi.Call{Args: []byte(`{}`)})
	if !strings.Contains(res.Content, "未应用 1") || !strings.Contains(res.Content, "#1 [已应用]") {
		t.Fatalf("应用后列表错误: %q", res.Content)
	}
	// 重复应用拒绝
	res, _ = invokeApply(ctx, h, &toolapi.Call{Args: []byte(`{"id":1}`)})
	if !res.IsError {
		t.Fatal("重复应用必须拒绝")
	}
	// 不存在编号
	res, _ = invokeApply(ctx, h, &toolapi.Call{Args: []byte(`{"id":99}`)})
	if !res.IsError {
		t.Fatal("不存在编号必须拒绝")
	}
}
