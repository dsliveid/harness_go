package events

import (
	"encoding/json"
	"testing"
	"time"
)

type deltaPayload struct {
	Delta string `json:"delta"`
}

func TestPublishAssignsMonotonicSeq(t *testing.T) {
	bus := NewBus(10, 4)
	defer bus.Close()

	for i := 0; i < 3; i++ {
		ev, err := bus.Publish("r1", TypeMessageDelta, deltaPayload{Delta: "x"})
		if err != nil {
			t.Fatalf("发布失败: %v", err)
		}
		if ev.Seq != int64(i+1) {
			t.Fatalf("seq 应递增为 %d，得到 %d", i+1, ev.Seq)
		}
		if ev.V != ProtocolVersion {
			t.Fatalf("事件必须带协议版本，得到 %d", ev.V)
		}
	}
	if got := bus.LastSeq("r1"); got != 3 {
		t.Fatalf("LastSeq 应为 3，得到 %d", got)
	}
}

func TestSubscribeReceivesNewEvents(t *testing.T) {
	bus := NewBus(10, 4)
	defer bus.Close()

	sub, err := bus.Subscribe("r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Unsubscribe(sub)

	if _, err := bus.Publish("r1", TypeMessageDelta, deltaPayload{Delta: "hello"}); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-sub.C():
		if ev.Type != TypeMessageDelta {
			t.Fatalf("事件类型错误: %s", ev.Type)
		}
		var p deltaPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Delta != "hello" {
			t.Fatalf("payload 错误: %+v", p)
		}
	case <-time.After(time.Second):
		t.Fatal("超时未收到事件")
	}
}

// Last-Event-ID 断线续传：只补发 seq > fromSeq 的历史。
func TestSubscribeReplaysFromSeq(t *testing.T) {
	bus := NewBus(100, 16)
	defer bus.Close()

	for i := 0; i < 5; i++ {
		if _, err := bus.Publish("r1", TypeMessageDelta, deltaPayload{Delta: "e"}); err != nil {
			t.Fatal(err)
		}
	}

	sub, err := bus.Subscribe("r1", 2) // 只要 seq 3,4,5
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Unsubscribe(sub)

	var got []int64
	for i := 0; i < 3; i++ {
		select {
		case ev := <-sub.C():
			got = append(got, ev.Seq)
		case <-time.After(time.Second):
			t.Fatalf("超时，仅收到 %v", got)
		}
	}
	want := []int64{3, 4, 5}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("补发序列错误: 期望 %v，得到 %v", want, got)
		}
	}
}

// 架构验证 A4：慢消费者（不消费）不得阻塞 Run，且被主动断开。
func TestSlowConsumerDoesNotBlockPublish(t *testing.T) {
	const subBuf = 2
	bus := NewBus(1000, subBuf)
	defer bus.Close()

	sub, err := bus.Subscribe("r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	// 故意不消费：发布远超缓冲容量的事件
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			if _, err := bus.Publish("r1", TypeMessageDelta, deltaPayload{Delta: "flood"}); err != nil {
				t.Errorf("发布失败: %v", err)
				break
			}
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish 被慢消费者阻塞了（违反背压铁律）")
	}

	if got := bus.SubscriberCount("r1"); got != 0 {
		t.Fatalf("慢消费者应被断开，仍有 %d 个订阅者", got)
	}
	if sub.CloseReason() != ReasonSlowConsumer {
		t.Fatalf("断开原因应为 slow_consumer，得到 %q", sub.CloseReason())
	}
	// 通道已关闭：先排空缓冲中残留的事件，最终必须读到关闭信号
	drained := 0
	for {
		if _, ok := <-sub.C(); !ok {
			break
		}
		drained++
		if drained > subBuf+1 {
			t.Fatal("通道未关闭（残留事件超出缓冲上限）")
		}
	}
	if drained > subBuf {
		t.Fatalf("残留事件不应超过订阅缓冲 %d，实际 %d", subBuf, drained)
	}
}

// 环形缓冲只保留最近 ringSize 条：过老的事件补发不到（客户端需走 resync）。
func TestRingBufferTrimsOldEvents(t *testing.T) {
	const ring = 3
	bus := NewBus(ring, 64)
	defer bus.Close()

	for i := 0; i < 10; i++ {
		if _, err := bus.Publish("r1", TypeMessageDelta, nil); err != nil {
			t.Fatal(err)
		}
	}

	sub, err := bus.Subscribe("r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	// fromSeq=0 不补发历史；用 fromSeq=1 验证只拿到 ring 内最近 3 条（8,9,10）
	bus.Unsubscribe(sub)

	sub2, err := bus.Subscribe("r1", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Unsubscribe(sub2)

	var seqs []int64
	for i := 0; i < ring; i++ {
		select {
		case ev := <-sub2.C():
			seqs = append(seqs, ev.Seq)
		case <-time.After(time.Second):
			t.Fatalf("超时，收到 %v", seqs)
		}
	}
	want := []int64{8, 9, 10}
	for i := range want {
		if seqs[i] != want[i] {
			t.Fatalf("ring 应只保留最近 %d 条: 期望 %v，得到 %v", ring, want, seqs)
		}
	}
}

func TestCloseRunNotifiesSubscribers(t *testing.T) {
	bus := NewBus(10, 4)
	defer bus.Close()

	sub, err := bus.Subscribe("r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	bus.CloseRun("r1")

	select {
	case _, ok := <-sub.C():
		if ok {
			t.Fatal("CloseRun 后通道应关闭")
		}
	case <-time.After(time.Second):
		t.Fatal("CloseRun 未通知订阅者")
	}
	if sub.CloseReason() != ReasonRunClosed {
		t.Fatalf("关闭原因错误: %q", sub.CloseReason())
	}
	// 关闭后不可再发布
	if _, err := bus.Publish("r1", TypeRunStatus, nil); err == nil {
		t.Fatal("已关闭的 Run 不应允许发布")
	}
	// 关闭后订阅：补发历史后立即关闭
	sub2, err := bus.Subscribe("r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := <-sub2.C(); ok {
		t.Fatal("已结束 Run 的新订阅应立即关闭")
	}
}

func TestMultipleSubscribersAllReceive(t *testing.T) {
	bus := NewBus(10, 8)
	defer bus.Close()

	subs := make([]*Subscriber, 3)
	for i := range subs {
		s, err := bus.Subscribe("r1", 0)
		if err != nil {
			t.Fatal(err)
		}
		subs[i] = s
	}
	if got := bus.SubscriberCount("r1"); got != 3 {
		t.Fatalf("订阅者数应为 3，得到 %d", got)
	}

	if _, err := bus.Publish("r1", TypeRunStatus, nil); err != nil {
		t.Fatal(err)
	}
	for i, s := range subs {
		select {
		case ev := <-s.C():
			if ev.Seq != 1 {
				t.Fatalf("订阅者 %d 收到错误事件: %+v", i, ev)
			}
		case <-time.After(time.Second):
			t.Fatalf("订阅者 %d 未收到事件", i)
		}
	}
}

func TestPublishRejectsEmptyRunID(t *testing.T) {
	bus := NewBus(10, 4)
	defer bus.Close()
	if _, err := bus.Publish("", TypeRunStatus, nil); err == nil {
		t.Fatal("空 runID 应报错")
	}
	if _, err := bus.Subscribe("", 0); err == nil {
		t.Fatal("空 runID 订阅应报错")
	}
}
