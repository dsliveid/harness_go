package device

import (
	"sync"
	"testing"
	"time"
)

// stateBusClock 可注入时钟：状态总线的时间断言必须可复现。
type stateBusClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stateBusClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stateBusClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestBus(t *testing.T) (*StateBus, *stateBusClock) {
	t.Helper()
	clock := &stateBusClock{t: time.Unix(1700000000, 0)}
	return NewStateBus(StateBusOptions{Now: clock.now}), clock
}

// recv 阻塞读一条事件（带超时防挂死）。
func recv(t *testing.T, sub StateSubscription, timeout time.Duration) (DeviceStateEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-sub.C():
		return ev, ok
	case <-time.After(timeout):
		t.Fatalf("等待事件超时（%s）", timeout)
		return DeviceStateEvent{}, false
	}
}

// expectNoEvent 断言在给定窗口内**没有**事件到达。
func expectNoEvent(t *testing.T, sub StateSubscription, window time.Duration) {
	t.Helper()
	select {
	case ev, ok := <-sub.C():
		if ok {
			t.Fatalf("不应收到事件，却收到了 %+v", ev)
		}
	case <-time.After(window):
	}
}

func TestStateBusSnapshotEmptyAndSorted(t *testing.T) {
	b, _ := newTestBus(t)
	if got := b.Snapshot(); len(got) != 0 {
		t.Fatalf("空总线快照应为空，得到 %d 条", len(got))
	}

	// 乱序上线三台，快照必须按 device_id 稳定排序。
	b.MarkOnline("dev-3", DeviceState{Name: "c"})
	b.MarkOnline("dev-1", DeviceState{Name: "a"})
	b.MarkOnline("dev-2", DeviceState{Name: "b"})

	got := b.Snapshot()
	if len(got) != 3 {
		t.Fatalf("应有 3 台设备，得到 %d", len(got))
	}
	want := []string{"dev-1", "dev-2", "dev-3"}
	for i, id := range want {
		if got[i].DeviceID != id {
			t.Fatalf("快照顺序错误：第 %d 位应为 %s，得到 %s", i, id, got[i].DeviceID)
		}
	}
}

func TestStateBusMarkOnlineBroadcastsOnce(t *testing.T) {
	b, clock := newTestBus(t)
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	b.MarkOnline("dev-1", DeviceState{Name: "手机", Kind: "mobile"})
	ev, ok := recv(t, sub, time.Second)
	if !ok {
		t.Fatal("订阅通道意外关闭")
	}
	if ev.Type != StateEventDeviceOnline {
		t.Fatalf("事件类型应为 %s，得到 %s", StateEventDeviceOnline, ev.Type)
	}
	if ev.Device == nil || ev.Device.DeviceID != "dev-1" {
		t.Fatalf("事件应携带 dev-1，得到 %+v", ev.Device)
	}
	if !ev.Device.Online {
		t.Fatal("上线事件的 Online 必须为 true")
	}

	// 幂等：同一设备重复上线**不再广播**（否则端侧重连会把观察侧刷屏）。
	clock.advance(time.Second)
	b.MarkOnline("dev-1", DeviceState{})
	expectNoEvent(t, sub, 150*time.Millisecond)

	// 但 last_seen 必须被刷新。
	snap := b.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("应仍有 1 台设备，得到 %d", len(snap))
	}
	if snap[0].LastSeenAt != clock.now().UnixMilli() {
		t.Fatalf("重复上线应刷新 last_seen：want %d, got %d",
			clock.now().UnixMilli(), snap[0].LastSeenAt)
	}
}

func TestStateBusMarkOfflineOnlyWhenOnline(t *testing.T) {
	b, _ := newTestBus(t)
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	// 未在册的设备离线 → 不广播（避免幽灵 offline 事件）。
	b.MarkOffline("ghost")
	expectNoEvent(t, sub, 150*time.Millisecond)

	b.MarkOnline("dev-1", DeviceState{Name: "手机"})
	_, _ = recv(t, sub, time.Second) // 消费 online

	b.MarkOffline("dev-1")
	ev, ok := recv(t, sub, time.Second)
	if !ok {
		t.Fatal("订阅通道意外关闭")
	}
	if ev.Type != StateEventDeviceOffline {
		t.Fatalf("事件类型应为 %s，得到 %s", StateEventDeviceOffline, ev.Type)
	}
	if ev.Device == nil || ev.Device.Online {
		t.Fatal("离线事件的 Online 必须为 false")
	}
	// 离线时协议版本必须清零（它只描述「当前连接」）。
	if ev.Device.ProtocolVersion != 0 {
		t.Fatalf("离线后协议版本应清零，得到 %d", ev.Device.ProtocolVersion)
	}

	// 重复离线 → 不广播。
	b.MarkOffline("dev-1")
	expectNoEvent(t, sub, 150*time.Millisecond)
}

func TestStateBusOfflineThenOnlineRebroadcasts(t *testing.T) {
	b, _ := newTestBus(t)
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	b.MarkOnline("dev-1", DeviceState{Name: "手机"})
	_, _ = recv(t, sub, time.Second)
	b.MarkOffline("dev-1")
	_, _ = recv(t, sub, time.Second)

	// 从离线转回在线**必须**广播（这是真实的状态跃迁）。
	b.MarkOnline("dev-1", DeviceState{Name: "手机"})
	ev, ok := recv(t, sub, time.Second)
	if !ok {
		t.Fatal("订阅通道意外关闭")
	}
	if ev.Type != StateEventDeviceOnline {
		t.Fatalf("重新上线应广播 %s，得到 %s", StateEventDeviceOnline, ev.Type)
	}
}

func TestStateBusTouchDoesNotBroadcast(t *testing.T) {
	b, clock := newTestBus(t)
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	b.MarkOnline("dev-1", DeviceState{})
	_, _ = recv(t, sub, time.Second)

	// 心跳只刷时间戳，绝不广播（否则每 15s 刷屏一次无意义事件）。
	clock.advance(15 * time.Second)
	b.Touch("dev-1")
	expectNoEvent(t, sub, 150*time.Millisecond)

	snap := b.Snapshot()
	if snap[0].LastSeenAt != clock.now().UnixMilli() {
		t.Fatalf("Touch 应刷新 last_seen：want %d, got %d",
			clock.now().UnixMilli(), snap[0].LastSeenAt)
	}
}

func TestStateBusSlowConsumerIsDroppedNotBlocking(t *testing.T) {
	// 缓冲设小，便于快速触发慢消费者。
	b := NewStateBus(StateBusOptions{BufferSize: 2})
	slow := b.Subscribe()
	defer b.Unsubscribe(slow)

	// 发布远多于缓冲的事件：若 publish 阻塞，本测试会挂死（这正是要防的：
	// 阻塞发布方 = 阻塞 WS 网关的 OnOnline/OnOffline 回调）。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			b.MarkOnline(deviceIDFor(i), DeviceState{})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("发布被慢消费者阻塞：publish 必须非阻塞")
	}

	// 慢消费者应被移除。
	if n := b.SubscriberCount(); n != 0 {
		t.Fatalf("慢消费者应被移除，剩余订阅者 %d", n)
	}
}

func deviceIDFor(i int) string {
	// 生成稳定且互不相同的 device id（避免 abcdef 拼装的噪音）。
	const alphabet = "0123456789"
	if i < len(alphabet) {
		return "dev-" + string(alphabet[i])
	}
	return "dev-x"
}

func TestStateBusUnsubscribeIdempotent(t *testing.T) {
	b, _ := newTestBus(t)
	sub := b.Subscribe()
	b.Unsubscribe(sub)
	b.Unsubscribe(sub) // 第二次必须无害

	if n := b.SubscriberCount(); n != 0 {
		t.Fatalf("退订后订阅者应为 0，得到 %d", n)
	}
	// 退订后通道应已关闭。
	if _, ok := <-sub.C(); ok {
		t.Fatal("退订后通道应关闭")
	}
}

func TestStateBusEmptyDeviceIDIgnored(t *testing.T) {
	b, _ := newTestBus(t)
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	b.MarkOnline("", DeviceState{})
	b.MarkOffline("")
	b.Touch("")

	expectNoEvent(t, sub, 100*time.Millisecond)
	if len(b.Snapshot()) != 0 {
		t.Fatalf("空 deviceID 不应入册，快照得到 %d 条", len(b.Snapshot()))
	}
}

func TestStateBusMarkOnlineKeepsPrevFieldsWhenBlank(t *testing.T) {
	b, _ := newTestBus(t)
	b.MarkOnline("dev-1", DeviceState{Name: "手机", Kind: "mobile"})

	// 重复上线但只带空字段：不应把已有 name/kind 抹成空。
	b.MarkOnline("dev-1", DeviceState{Name: "", Kind: ""})

	snap := b.Snapshot()
	if snap[0].Name != "手机" {
		t.Fatalf("空 Name 不应覆盖已有值，得到 %q", snap[0].Name)
	}
	if snap[0].Kind != "mobile" {
		t.Fatalf("空 Kind 不应覆盖已有值，得到 %q", snap[0].Kind)
	}
}

// TestStateBusRemoveBroadcasts 解绑（吊销）必须广播 device.revoked 并从快照移除。
//
// 为什么不能用 device.offline 代替：离线是「暂时连不上，仍在绑定中」，前端应保留
// 卡片并标灰；吊销是「绑定关系终止」，前端必须删除卡片。混用会让管理端点了
// 「解除绑定」后设备仍留在列表里 —— 这正是用户报告的 bug。
func TestStateBusRemoveBroadcasts(t *testing.T) {
	b, clock := newTestBus(t)
	b.MarkOnline("dev-1", DeviceState{Name: "我的桌面端", Kind: "desktop", UserID: "usr-1"})
	b.MarkOnline("dev-2", DeviceState{Name: "手机", Kind: "mobile"})

	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	b.Remove("dev-1")

	ev, ok := recv(t, sub, time.Second)
	if !ok {
		t.Fatal("Remove 应广播事件")
	}
	if ev.Type != StateEventDeviceRevoked {
		t.Fatalf("事件类型应为 %s，得到 %s", StateEventDeviceRevoked, ev.Type)
	}
	if ev.Device == nil || ev.Device.DeviceID != "dev-1" {
		t.Fatalf("事件应携带被移除设备的 ID，得到 %+v", ev.Device)
	}
	// 展示字段随事件带出，便于前端在移除前提示「已解绑"我的桌面端"」
	if ev.Device.Name != "我的桌面端" {
		t.Fatalf("事件应带上已知的设备名，得到 %q", ev.Device.Name)
	}
	if ev.At != clock.now().UnixMilli() {
		t.Fatalf("事件时间应为当前时钟，得到 %d", ev.At)
	}

	// 快照里必须消失，且只删指定的那台
	snap := b.Snapshot()
	if len(snap) != 1 || snap[0].DeviceID != "dev-2" {
		t.Fatalf("移除后快照应只剩 dev-2，得到 %+v", snap)
	}
}

// TestStateBusRemoveUnregisteredStillBroadcasts 不在册的设备也必须广播。
//
// 场景：管理端吊销一台「从未建立过长连接」的设备（StateBus 里没有它的条目）。
// 它的数据只存在于其他浏览器 GET /devices 的结果里 —— 若因本地不在册就不广播，
// 那些页面的列表永远不会消失。
func TestStateBusRemoveUnregisteredStillBroadcasts(t *testing.T) {
	b, _ := newTestBus(t)
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)

	b.Remove("dev-ghost")

	ev, ok := recv(t, sub, time.Second)
	if !ok {
		t.Fatal("不在册设备的 Remove 也必须广播（否则其他页面列表不收敛）")
	}
	if ev.Type != StateEventDeviceRevoked || ev.Device == nil || ev.Device.DeviceID != "dev-ghost" {
		t.Fatalf("事件载荷错误: %+v", ev)
	}
	if len(b.Snapshot()) != 0 {
		t.Fatal("幽灵设备移除后快照仍应为空（不应凭空入册）")
	}

	// 空 ID 忽略
	b.Remove("")
	expectNoEvent(t, sub, 100*time.Millisecond)
}
