package memory

import (
	"context"
	"strings"
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

// indexerHost 给 plugintest.Host 加装 MemoryIndexer 能力（模拟内核已接线）。
//
// 记录每次被要求同步的 (layer, fileName)，用于断言"写后确实推了索引"。
type indexerHost struct {
	*plugintest.Host
	calls   []string
	succeed bool
}

func (h *indexerHost) IndexMemoryFile(_ context.Context, layer, fileName string) (bool, string) {
	h.calls = append(h.calls, layer+"/"+fileName)
	if h.succeed {
		return true, "已进索引"
	}
	return false, "索引未变更（内容与索引一致）"
}

// TestRecordMemorySyncsIndexOnWrite 落盘成功后必须立刻同步索引。
//
// 这是"边读边记"能否在**单次对话内**生效的关键：改造前索引只在进程启动时
// 同步一次，会话中新写的记忆要等重启才召回得到。
func TestRecordMemorySyncsIndexOnWrite(t *testing.T) {
	h := &indexerHost{Host: plugintest.New(t), succeed: true}
	res, err := recordMemory(context.Background(), h, &toolapi.Call{Args: []byte(
		`{"layer":"notes","title":"写后同步","body":"内容"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 1 {
		t.Fatalf("写后应同步一次索引，实际 %d 次: %v", len(h.calls), h.calls)
	}
	if !strings.HasPrefix(h.calls[0], "notes/") {
		t.Fatalf("同步的层/文件不符: %v", h.calls)
	}
	// 回执必须如实说明"已进索引"与"何时可召回"
	if !strings.Contains(res.Content, "已同步进检索索引") {
		t.Fatalf("回执应说明已进索引: %q", res.Content)
	}
	if !strings.Contains(res.Content, "下一轮起可被召回") {
		t.Fatalf("回执应说明生效时机: %q", res.Content)
	}
	// 必须明确"本轮不重复注入"——这是防止模型误以为"刚写的会立刻回到自己上下文"
	if !strings.Contains(res.Content, "不重复注入") {
		t.Fatalf("回执应说明本轮不重复注入: %q", res.Content)
	}
}

// TestRecordMemoryWithoutIndexerIsHonest 未接线时必须如实说"仅落盘"，绝不假装已进索引。
func TestRecordMemoryWithoutIndexerIsHonest(t *testing.T) {
	h := plugintest.New(t) // 不实现 MemoryIndexer
	res, err := recordMemory(context.Background(), h, &toolapi.Call{Args: []byte(
		`{"layer":"notes","title":"无索引器","body":"内容"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "已同步进检索索引") {
		t.Fatalf("未接线时不得声称已进索引: %q", res.Content)
	}
	if !strings.Contains(res.Content, "未接线") {
		t.Fatalf("应如实说明未接线: %q", res.Content)
	}
}

// TestRecordMemoryWriteFailureDoesNotIndex 写盘失败时不得尝试同步索引。
//
// 否则索引里会出现一条"文件根本不存在"的记忆，召回后模型读到空内容。
func TestRecordMemoryWriteFailureDoesNotIndex(t *testing.T) {
	h := &indexerHost{Host: plugintest.New(t), succeed: true}
	// 用非法层触发早退（在写盘之前）
	res, _ := recordMemory(context.Background(), h, &toolapi.Call{Args: []byte(
		`{"layer":"bogus","title":"x","body":"y"}`)})
	if !res.IsError {
		t.Fatal("非法层必须拒绝")
	}
	if len(h.calls) != 0 {
		t.Fatalf("校验失败时不得同步索引，实际 %v", h.calls)
	}
}
