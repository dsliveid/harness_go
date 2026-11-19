package memory

import (
	"context"
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

func TestRecordAndReadMemory(t *testing.T) {
	h := plugintest.New(t)
	ctx := context.Background()
	res, err := recordMemory(ctx, h, &toolapi.Call{Args: []byte(
		`{"layer":"notes","title":"部署结论","body":"本机部署用 docker compose v2","importance":0.8}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "notes/") {
		t.Fatalf("应返回相对路径: %q", res.Content)
	}
	// 回执必须如实说明"何时可用"（改造前误称"下次启动"）
	if !strings.Contains(res.Content, "下一轮起可被召回") {
		t.Fatalf("回执未交代生效时机: %q", res.Content)
	}
	// 列清单（无 query）
	res, _ = readMemory(ctx, h, &toolapi.Call{Args: []byte(`{"layer":"notes"}`)})
	if !strings.Contains(res.Content, "notes/") {
		t.Fatalf("列表错误: %q", res.Content)
	}
	// 非法层拒绝
	res, _ = recordMemory(ctx, h, &toolapi.Call{Args: []byte(`{"layer":"l9","title":"x","body":"y"}`)})
	if !res.IsError {
		t.Fatal("非法层必须拒绝")
	}
}

// TestReadMemoryByQuery read_memory 支持按关键词检索（这是"记忆检索"的核心能力，
// 旧 memory_list 只能列文件清单，模型拿到文件名也读不到内容）。
func TestReadMemoryByQuery(t *testing.T) {
	h := plugintest.New(t)
	ctx := context.Background()
	for _, body := range []struct{ title, content string }{
		{"部署结论", "本机部署用 docker compose v2"},
		{"检索结论", "向量检索走 FTS5 + 余弦混合"},
	} {
		if _, err := recordMemory(ctx, h, &toolapi.Call{Args: []byte(
			`{"layer":"notes","title":"` + body.title + `","body":"` + body.content + `"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	res, _ := readMemory(ctx, h, &toolapi.Call{Args: []byte(`{"query":"向量检索"}`)})
	if !strings.Contains(res.Content, "命中 1 条") || !strings.Contains(res.Content, "检索结论") {
		t.Fatalf("关键词检索错误: %q", res.Content)
	}
	// 未命中必须如实报告，并提示可省略 query 列出全部
	res, _ = readMemory(ctx, h, &toolapi.Call{Args: []byte(`{"query":"不存在的关键词xyz"}`)})
	if !strings.Contains(res.Content, "没有记忆命中") {
		t.Fatalf("未命中口径错误: %q", res.Content)
	}
}

// TestMemoryManifestToolNames 清单必须是 record_memory / read_memory。
func TestMemoryManifestToolNames(t *testing.T) {
	tools := Manifest().Tools
	if len(tools) != 2 {
		t.Fatalf("应声明 2 个工具，实际 %d", len(tools))
	}
	got := map[string]bool{}
	for _, s := range tools {
		got[s.Name] = true
	}
	for _, want := range []string{"record_memory", "read_memory"} {
		if !got[want] {
			t.Fatalf("缺少工具 %s（实际: %v）", want, got)
		}
	}
}
