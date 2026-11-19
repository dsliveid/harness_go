package sessionbus

import (
	"sync"
	"testing"
	"time"
)

// waitEvent 在超时内读取一帧事件。
func waitEvent(t *testing.T, sub Subscription, timeout time.Duration) Event {
	t.Helper()
	select {
	case ev, open := <-sub.C():
		if !open {
			t.Fatalf("订阅通道被关闭，期望收到事件")
		}
		return ev
	case <-time.After(timeout):
		t.Fatalf("等待事件超时（%s）", timeout)
		return Event{}
	}
}

// assertNoEvent 断言在给定时间内**没有**事件（用于验证合并/撤销）。
func assertNoEvent(t *testing.T, sub Subscription, d time.Duration) {
	t.Helper()
	select {
	case ev, open := <-sub.C():
		if !open {
			return // 通道关闭也算「无事件」，由别的断言负责
		}
		t.Fatalf("期望无事件，却收到 %+v", ev)
	case <-time.After(d):
	}
}

// 合并窗口内的同会话多次 PublishUpdated 只产生一帧。
//
// 这是本总线最重要的契约：total_tokens / last_message_at 在每条消息落库时都会
// 触发更新，若不去抖，一次 Run 会把列表刷新刷成几十帧。
func TestPublishUpdatedMergesWithinWindow(t *testing.T) {
	b := New(Options{MergeWindow: 60 * time.Millisecond})
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	for i := 0; i < 10; i++ {
		b.PublishUpdated("s-1")
	}

	ev := waitEvent(t, sub, time.Second)
	if ev.Type != EventUpdated || ev.SessionID != "s-1" {
		t.Fatalf("事件不符：%+v", ev)
	}

	// 窗口内后续的 10 次调用必须只结算一帧：不再有第二帧。
	assertNoEvent(t, sub, 150*time.Millisecond)
}

// 不同会话各自独立合并，不能互相吞并。
func TestPublishUpdatedDifferentSessionsNotMerged(t *testing.T) {
	b := New(Options{MergeWindow: 40 * time.Millisecond})
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	b.PublishUpdated("s-1")
	b.PublishUpdated("s-2")
	b.PublishUpdated("s-1")

	got := map[string]int{}
	for i := 0; i < 2; i++ {
		ev := waitEvent(t, sub, time.Second)
		got[ev.SessionID]++
	}
	if got["s-1"] != 1 || got["s-2"] != 1 {
		t.Fatalf("期望 s-1/s-2 各一帧，实际 %+v", got)
	}
	assertNoEvent(t, sub, 100*time.Millisecond)
}

// 合并窗口结束后再次调用必须产生新的一帧（不能因为上次已处理就永久静默）。
func TestPublishUpdatedAfterWindowEmitsAgain(t *testing.T) {
	b := New(Options{MergeWindow: 30 * time.Millisecond})
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	b.PublishUpdated("s-1")
	waitEvent(t, sub, time.Second)

	b.PublishUpdated("s-1")
	ev := waitEvent(t, sub, time.Second)
	if ev.Type != EventUpdated || ev.SessionID != "s-1" {
		t.Fatalf("第二帧不符：%+v", ev)
	}
}

// MergeWindow<=0 时退化为「立即广播」，便于测试与本地调试。
func TestMergeWindowZeroEmitsImmediately(t *testing.T) {
	b := New(Options{MergeWindow: -1})
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	b.PublishUpdated("s-1")
	ev := waitEvent(t, sub, 100*time.Millisecond)
	if ev.Type != EventUpdated {
		t.Fatalf("期望立即 updated，实际 %+v", ev)
	}
}

// PublishImmediate 不受合并窗口影响。
func TestPublishImmediateBypassesWindow(t *testing.T) {
	b := New(Options{MergeWindow: time.Hour})
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	b.PublishImmediate("s-9", EventUpdated)
	ev := waitEvent(t, sub, 200*time.Millisecond)
	if ev.SessionID != "s-9" || ev.Type != EventUpdated {
		t.Fatalf("事件不符：%+v", ev)
	}
}

// PublishRemoved 立即广播，且撤销该会话尚未结算的 updated 帧。
//
// 若不撤销，窗口结束时会再吐一帧 updated，让客户端去回查一个已删除的会话。
func TestPublishRemovedCancelsPendingUpdate(t *testing.T) {
	b := New(Options{MergeWindow: 60 * time.Millisecond})
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	b.PublishUpdated("s-1")
	b.PublishRemoved("s-1")

	ev := waitEvent(t, sub, time.Second)
	if ev.Type != EventRemoved {
		t.Fatalf("期望 removed 优先，实际 %+v", ev)
	}
	// 窗口到期后不得再出现被撤销的 updated。
	assertNoEvent(t, sub, 150*time.Millisecond)
}

// 空 sessionID 不得广播（防埋点误传空值把全端刷成空帧）。
func TestEmptySessionIDIgnored(t *testing.T) {
	b := New(Options{MergeWindow: -1})
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	b.PublishUpdated("")
	b.PublishImmediate("", EventUpdated)
	b.PublishRemoved("")
	assertNoEvent(t, sub, 100*time.Millisecond)
	if n := b.SubscriberCount(); n != 1 {
		t.Fatalf("订阅者数应为 1，实际 %d", n)
	}
}

// 慢消费者：缓冲满即断开，且**绝不阻塞发布方**。
func TestSlowSubscriberDroppedWithoutBlocking(t *testing.T) {
	b := New(Options{BufferSize: 1, MergeWindow: -1})
	b.Subscribe() // 故意不消费，制造慢消费者

	done := make(chan struct{})
	go func() {
		defer close(done)
		// 远多于缓冲容量：若 publish 会阻塞，这个 goroutine 永远结束不了。
		for i := 0; i < 50; i++ {
			b.PublishImmediate("s-1", EventUpdated)
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("发布方被阻塞：慢消费者策略失效")
	}

	// 被断开后订阅者应从总线移除。
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if b.SubscriberCount() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := b.SubscriberCount(); n != 0 {
		t.Fatalf("慢消费者应被移除，实际仍有 %d 个订阅者", n)
	}
}

// 取消订阅幂等，且关闭通道。
func TestUnsubscribeIdempotent(t *testing.T) {
	b := New(Options{})
	sub := b.Subscribe()
	b.Unsubscribe(sub)
	b.Unsubscribe(sub)
	b.Unsubscribe(nil)

	if _, open := <-sub.C(); open {
		t.Fatal("取消订阅后通道应已关闭")
	}
	if n := b.SubscriberCount(); n != 0 {
		t.Fatalf("订阅者数应为 0，实际 %d", n)
	}
}

// 并发发布 + 并发订阅/退订不得竞态（配合 -race 更有意义）。
func TestConcurrentPublishAndSubscribe(t *testing.T) {
	b := New(Options{MergeWindow: time.Millisecond})
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				b.PublishUpdated("s-1")
				b.PublishImmediate("s-2", EventUpdated)
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s := b.Subscribe()
				// 不消费，故意制造被断开的慢消费者路径
				b.Unsubscribe(s)
			}
		}()
	}
	wg.Wait()
}
