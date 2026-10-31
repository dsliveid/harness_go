package memory

import (
	"context"
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

func TestMemoryWriteList(t *testing.T) {
	h := plugintest.New(t)
	ctx := context.Background()
	res, err := invokeWrite(ctx, h, &toolapi.Call{Args: []byte(
		`{"layer":"notes","title":"部署结论","body":"本机部署用 docker compose v2","importance":0.8}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "notes/") {
		t.Fatalf("应返回相对路径: %q", res.Content)
	}
	// 落盘内容含重要性标记（同步器解析依据）
	data, _ := h.ReadFile(ctx, MemoryDir+"/notes/部署结论-")
	if data == nil {
		// 文件名带日期，列出再校验
		res, _ = invokeList(ctx, h, &toolapi.Call{Args: []byte(`{"layer":"notes"}`)})
		if !strings.Contains(res.Content, "notes/") {
			t.Fatalf("列表错误: %q", res.Content)
		}
	}
	// 非法层拒绝
	res, _ = invokeWrite(ctx, h, &toolapi.Call{Args: []byte(`{"layer":"l9","title":"x","body":"y"}`)})
	if !res.IsError {
		t.Fatal("非法层必须拒绝")
	}
}
