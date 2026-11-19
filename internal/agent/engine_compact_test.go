package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"harness-go/internal/llm"
	"harness-go/internal/model"
	"harness-go/internal/prompt"
)

// fakeCompactor 压缩端口桩：记录调用并返回可控结果。
type fakeCompactor struct {
	calls      int
	// needsCalls 记录 NeedsCompaction 被问过多少次。
	needsCalls int
	// needs 固定返回的水位判断结果（默认 false = 不需要压缩）。
	needs bool
	result     CompactResult
	err        error
	historyErr error
	lastReq    CompactRequest
	// historyOverride 非 nil 时直接返回它（用于验证"压缩后重建历史"）。
	historyOverride []llm.Message
}

func (f *fakeCompactor) NeedsCompaction(used, limit int) bool {
	f.needsCalls++
	// 复刻真实实现的关键契约：窗口未知时必须返回 false。
	if limit <= 0 {
		return false
	}
	return f.needs
}

func (f *fakeCompactor) Compact(_ context.Context, req CompactRequest) (CompactResult, error) {
	f.calls++
	f.lastReq = req
	return f.result, f.err
}

func (f *fakeCompactor) History(context.Context, string, []model.Message) ([]llm.Message, error) {
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	if f.historyOverride != nil {
		return f.historyOverride, nil
	}
	return []llm.Message{{Role: llm.RoleUser, Content: "摘要"}}, nil
}

// TestTryCompactSkippedWhenNoCompactor 无压缩器时不得 panic、不得改变历史。
func TestTryCompactSkippedWhenNoCompactor(t *testing.T) {
	eng, sdb, _, _ := newTestEngine(t, &fakeProvider{})
	history := []llm.Message{{Role: llm.RoleUser, Content: "原历史"}}

	got, used := eng.tryCompact(context.Background(), sdb, baseRunRequest(),
		prompt.BuildResult{}, history)
	if used {
		t.Error("无压缩器时应返回 false")
	}
	if got != nil {
		t.Error("未压缩时不应返回新历史")
	}
}

// TestTryCompactFailureDoesNotPropagate 压缩失败不得向上抛错（优化项语义）。
func TestTryCompactFailureDoesNotPropagate(t *testing.T) {
	comp := &fakeCompactor{err: errors.New("model down")}
	eng, sdb, _, _ := newTestEngine(t, &fakeProvider{}, func(d *Deps) {
		d.Compactor = comp
		d.MaxContextTokens = 100000
	})

	got, used := eng.tryCompact(context.Background(), sdb, baseRunRequest(),
		prompt.BuildResult{}, nil)
	if used {
		t.Error("压缩失败时应返回 false")
	}
	if got != nil {
		t.Error("压缩失败时不应返回新历史")
	}
}

// TestTryCompactPassesRunningTrue 引擎调用压缩时必须声明 Running=true。
//
// 这条断言锁定"活跃轮次保护"的前提：若这里传 false，
// 压缩器会误以为会话空闲，从而折叠当前正在进行的任务。
func TestTryCompactPassesRunningTrue(t *testing.T) {
	comp := &fakeCompactor{result: CompactResult{Compacted: false}}
	eng, sdb, _, _ := newTestEngine(t, &fakeProvider{}, func(d *Deps) {
		d.Compactor = comp
		d.MaxContextTokens = 100000
	})

	_, _ = eng.tryCompact(context.Background(), sdb, baseRunRequest(), prompt.BuildResult{}, nil)

	if comp.calls != 1 {
		t.Fatalf("应调用 1 次压缩，得到 %d", comp.calls)
	}
	if !comp.lastReq.Running {
		t.Fatal("引擎必须传 Running=true（否则压缩器会折叠当前活跃任务）")
	}
	if comp.lastReq.Limit != 100000 {
		t.Errorf("应传递上下文窗口 100000，得到 %d", comp.lastReq.Limit)
	}
}

// TestTryCompactPublishesEvent 压缩成功必须发事件（对用户可见）。
//
// 若压缩静默生效，用户会看到模型"莫名其妙忘了前面的事"却无从排查。
func TestTryCompactPublishesEvent(t *testing.T) {
	comp := &fakeCompactor{
		result: CompactResult{
			Compacted: true, StartSeq: 1, EndSeq: 10,
			ReplacedMessages: 10, SavedTokens: 5000,
		},
	}
	eng, sdb, bus, _ := newTestEngine(t, &fakeProvider{}, func(d *Deps) {
		d.Compactor = comp
		d.MaxContextTokens = 100000
	})

	sub, err := bus.Subscribe("run-1", -1)
	if err != nil {
		t.Fatalf("订阅失败: %v", err)
	}

	_, used := eng.tryCompact(context.Background(), sdb, baseRunRequest(), prompt.BuildResult{}, nil)
	if !used {
		t.Fatal("压缩成功应返回 true")
	}

	select {
	case ev := <-sub.C():
		var payload map[string]any
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("事件载荷解析失败: %v", err)
		}
		if payload["scope"] != "compaction" {
			t.Errorf("事件 scope 应为 compaction，得到 %v", payload["scope"])
		}
	case <-time.After(time.Second):
		t.Fatal("压缩成功必须发事件（否则用户无从知道历史被折叠）")
	}
}

// TestTryCompactHistoryFailureFallsBack 历史重建失败时退回未压缩（fail-safe）。
func TestTryCompactHistoryFailureFallsBack(t *testing.T) {
	comp := &fakeCompactor{
		result:     CompactResult{Compacted: true, EndSeq: 5},
		historyErr: errors.New("db down"),
	}
	eng, sdb, _, _ := newTestEngine(t, &fakeProvider{}, func(d *Deps) {
		d.Compactor = comp
		d.MaxContextTokens = 100000
	})

	got, used := eng.tryCompact(context.Background(), sdb, baseRunRequest(), prompt.BuildResult{}, nil)
	if used {
		t.Error("历史重建失败时应退回未压缩")
	}
	if got != nil {
		t.Error("不应返回半成品历史")
	}
}

// TestCompactionEventMessageIsReadable 事件文案必须是人能看懂的。
func TestCompactionEventMessageIsReadable(t *testing.T) {
	comp := &fakeCompactor{
		result: CompactResult{Compacted: true, StartSeq: 3, EndSeq: 20, ReplacedMessages: 18, SavedTokens: 12000},
	}
	eng, sdb, bus, _ := newTestEngine(t, &fakeProvider{}, func(d *Deps) {
		d.Compactor = comp
		d.MaxContextTokens = 100000
	})
	sub, err := bus.Subscribe("run-1", -1)
	if err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	_, _ = eng.tryCompact(context.Background(), sdb, baseRunRequest(), prompt.BuildResult{}, nil)

	select {
	case ev := <-sub.C():
		var payload map[string]any
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("事件载荷解析失败: %v", err)
		}
		msg, _ := payload["message"].(string)
		for _, must := range []string{"3", "20", "18", "12000"} {
			if !strings.Contains(msg, must) {
				t.Errorf("事件文案缺少关键信息 %q，实际: %q", must, msg)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("未收到事件")
	}
}
