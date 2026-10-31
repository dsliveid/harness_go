package events

// SubscribeAll 全局订阅（P1 Webhook 分发用）单测。

import (
	"sync"
	"testing"
	"time"
)

// TestSubscribeAllReceivesNewEvents 全局订阅收到所有 Run 的新事件、不补发历史。
func TestSubscribeAllReceivesNewEvents(t *testing.T) {
	b := NewBus(100, 16)
	defer b.Close()

	// 先发布历史（全局订阅不补发）
	if _, err := b.Publish("run-a", TypeRunStatus, map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	sub := b.SubscribeAll()
	defer b.Unsubscribe(sub)

	if _, err := b.Publish("run-b", TypeRunStatus, map[string]any{"n": 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish("run-c", TypeToolResult, map[string]any{"n": 3}); err != nil {
		t.Fatal(err)
	}

	got := make([]Event, 0, 2)
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case ev, open := <-sub.C():
			if !open {
				t.Fatal("订阅被意外关闭")
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("事件不足: %d", len(got))
		}
	}
	if got[0].RunID != "run-b" || got[1].RunID != "run-c" {
		t.Fatalf("应只收到新事件: %+v", got)
	}
	if !sub.Global {
		t.Fatal("订阅应标记 Global")
	}
}

// TestSubscribeAllSlowConsumer 慢消费者缓冲满即断开（不阻塞发布方）。
func TestSubscribeAllSlowConsumer(t *testing.T) {
	b := NewBus(100, 2)
	defer b.Close()

	sub := b.SubscribeAll()
	// 超缓冲发布，不消费
	for i := 0; i < 10; i++ {
		if _, err := b.Publish("run-x", TypeRunStatus, map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	// 发布方未被阻塞（走到这里即通过）；慢消费者应被标记关闭
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sub.CloseReason() == ReasonSlowConsumer {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("慢消费者未被断开: reason=%q", sub.CloseReason())
}

// TestSubscribeAllConcurrent 并发发布下全局订阅不丢不重（seq 按 Run 内单调）。
func TestSubscribeAllConcurrent(t *testing.T) {
	b := NewBus(500, 256)
	defer b.Close()

	sub := b.SubscribeAll()
	defer b.Unsubscribe(sub)

	const runs, perRun = 4, 25
	var wg sync.WaitGroup
	for r := 0; r < runs; r++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < perRun; i++ {
				_, _ = b.Publish(string(rune('a'+n))+"-run", TypeRunStatus, map[string]any{"i": i})
			}
		}(r)
	}
	wg.Wait()

	seen := map[string]int{}
	deadline := time.After(3 * time.Second)
	for len(seen) < runs || anyLess(seen, perRun) {
		select {
		case ev, open := <-sub.C():
			if !open {
				t.Fatalf("订阅提前关闭: got %v", seen)
			}
			seen[ev.RunID]++
		case <-deadline:
			t.Fatalf("事件丢失: 期望 %d×%d，得到 %v", runs, perRun, seen)
		}
	}
}

func anyLess(seen map[string]int, want int) bool {
	for _, n := range seen {
		if n < want {
			return true
		}
	}
	return false
}
