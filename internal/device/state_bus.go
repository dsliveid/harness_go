// Package device 的「设备状态广播」：把 Hub 的上下线/心跳变化转成可订阅的
// 状态快照流，供 Web 前端实时展示「当前哪些设备在线」。
//
// # 为什么不复用 internal/events.Bus
//
// events.Bus 是 **per-Run 序号总线**（`Publish(runID, type, payload)` 硬性要求
// runID 非空，`bus.go:175-177`），事件结构含 `RunID` 与「Run 内单调递增的 Seq」。
// 而设备状态：
//   - **没有 Run 归属**；
//   - 语义是「当前态」而非「事件流」——重连后只需要拿到最新快照，
//     不需要按 seq 补发历史（这与 SSE 的 Last-Event-ID 语义正相反）。
//
// 硬塞进 events.Bus 会导致「用一个没有 Run 的假 runID 污染序号空间」，
// 并让 /devices/events 的续传语义与 /runs/{rid}/events 混淆。因此独立成一套。
//
// # 设计口径（与 Hub 解耦）
//
// StateBus **不持有 Hub 引用**，只暴露 Publish/Subscribe。装配方（cmd）把
// Hub 的 OnOnline/OnOffline 回调与 StateBus.Publish 组合起来，这样：
//   - 单元测试可以独立测 StateBus（不需要起 WS 网关）；
//   - Hub 不知道 StateBus 存在（依赖方向不变）。
package device

import (
	"sort"
	"sync"
	"time"
)

// DeviceState 单台设备的对外状态快照。
//
// 字段刻意保持精简且**不含任何令牌**：token_hash 属敏感派生值，
// 绝不能泄漏到 Web 前端或日志（红线 R3）。
type DeviceState struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	// UserID 设备归属账号（devices.user_id，见 0011 迁移）。
	//
	// 为什么要暴露到观察侧：设备与账号是持久绑定关系，观察面（设备列表 / SSE）
	// 若看不到归属，多用户场景下前端就无法按账号划分设备视图，只能"全都显示"。
	// 一期单用户时它恒为同一个值，但接口位先留好，避免日后改动 SSE 契约。
	//
	// 为空串 = 归属未知（0011 之前配对的历史设备），前端应如实展示为"未绑定账号"
	// 而不是隐藏该设备 —— 它仍在正常工作，只是缺归属、不能自续会话。
	// 与 token_hash 不同：这是**非机密**的归属标识，不涉及任何凭据。
	UserID string `json:"user_id,omitempty"`
	// Online 当前是否持有活跃长连接。
	Online bool `json:"online"`
	// LastSeenAt 最近一次活动时刻（Unix 毫秒）。离线后仍保留，便于 UI 展示
	// 「最后在线时间」而不是只显示一个「离线」。
	LastSeenAt int64 `json:"last_seen_at"`
	// ProtocolVersion 端侧上报的协议版本（仅在线时有意义）。
	ProtocolVersion int `json:"protocol_version,omitempty"`
}

// DeviceStateEvent 一条状态变化通知。
type DeviceStateEvent struct {
	// Type 取值：
	//   - "snapshot"       订阅首帧的全量快照（Event 为空数组）
	//   - "device.online"  某设备上线
	//   - "device.offline" 某设备离线
	Type   string        `json:"type"`
	Device *DeviceState  `json:"device,omitempty"`
	All    []DeviceState `json:"all,omitempty"`
	At     int64         `json:"at"`
}

// 状态变化类型常量。
const (
	StateEventSnapshot      = "snapshot"
	StateEventDeviceOnline  = "device.online"
	StateEventDeviceOffline = "device.offline"
	// StateEventDeviceRevoked 设备被吊销（= 解除绑定）。
	//
	// 为什么必须单独一个事件而不是复用 device.offline：两者对前端的语义完全不同 ——
	// 离线只是「暂时连不上，稍后可能回来」，设备仍在绑定关系中（卡片保留、显示离线）；
	// 吊销是「绑定关系终止」，设备应**从列表消失**。若复用 offline，管理端点完
	// 「解除绑定」后前端只会把卡片标灰，用户依然看到设备存在（这正是本次修复的 bug）。
	StateEventDeviceRevoked = "device.revoked"
)

// StateSubscription 状态订阅句柄（跨包可见）。
//
// 为什么要导出一层接口：`StateBus` 在 device 包，而 SSE handler 在 api 包，
// 若 Subscribe 返回未导出类型，api 包无法持有它。
type StateSubscription interface {
	// C 事件通道（关闭表示被断开：慢消费者）。
	C() <-chan DeviceStateEvent
}

// stateSubscriber 单个订阅者。
type stateSubscriber struct {
	ch     chan DeviceStateEvent
	closed bool
	mu     sync.Mutex
}

// StateBus 设备状态广播总线。
//
// 并发安全。慢消费者策略与 events.Bus 一致：**缓冲满即丢弃该订阅**
// （绝不阻塞发布方，即绝不阻塞 WS 网关的生命周期回调）。
type StateBus struct {
	mu     sync.RWMutex
	states map[string]DeviceState
	subs   map[*stateSubscriber]struct{}
	now    func() time.Time
	// bufferSize 单订阅者缓冲；满则视为慢消费者并关闭其订阅。
	bufferSize int
}

// StateBusOptions 构造参数（全部可省略）。
type StateBusOptions struct {
	// BufferSize 单订阅者事件缓冲（默认 32）。设小是为了让慢消费者
	// 尽快被识别并断开，而不是无限积压内存。
	BufferSize int
	// Now 可注入时钟（测试用）。
	Now func() time.Time
}

// NewStateBus 构造状态总线。
func NewStateBus(opts StateBusOptions) *StateBus {
	size := opts.BufferSize
	if size <= 0 {
		size = 32
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &StateBus{
		states:     make(map[string]DeviceState),
		subs:       make(map[*stateSubscriber]struct{}),
		now:        now,
		bufferSize: size,
	}
}

// Snapshot 返回当前全部设备状态的稳定快照（按 DeviceID 排序，便于断言与 UI 差分）。
func (b *StateBus) Snapshot() []DeviceState {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.snapshotLocked()
}

func (b *StateBus) snapshotLocked() []DeviceState {
	out := make([]DeviceState, 0, len(b.states))
	for _, s := range b.states {
		out = append(out, s)
	}
	// 排序保证「同样的集合永远产出同样的顺序」：SSE 首帧与测试断言都依赖它。
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceID < out[j].DeviceID })
	return out
}

// MarkOnline 标记设备上线并广播。
//
// 幂等语义：若该设备**已经在册且 online==true**，只更新 LastSeenAt，
// **不重复广播** —— 避免端侧频繁重连把观察侧刷屏。
func (b *StateBus) MarkOnline(id string, info DeviceState) {
	if id == "" {
		return
	}
	b.mu.Lock()
	prev, existed := b.states[id]
	info.DeviceID = id
	info.Online = true
	info.LastSeenAt = b.now().UnixMilli()
	if !existed || !prev.Online {
		// 首次上线，或从离线转在线 → 需要广播。
		b.states[id] = info
		b.mu.Unlock()
		b.publish(DeviceStateEvent{
			Type:   StateEventDeviceOnline,
			Device: &info,
			At:     info.LastSeenAt,
		})
		return
	}
	// 已在线：只刷新 LastSeenAt / 协议版本，不广播。
	if info.Name == "" {
		info.Name = prev.Name
	}
	if info.Kind == "" {
		info.Kind = prev.Kind
	}
	// 归属同样沿用上一份快照：调用方（OnOnline 回调）可能因查询失败而没带上
	// UserID，此时**不能**把已有的归属抹成空串 —— 那会让前端误以为设备"刚解除绑定"。
	if info.UserID == "" {
		info.UserID = prev.UserID
	}
	b.states[id] = info
	b.mu.Unlock()
}

// MarkOffline 标记设备离线并广播。
//
// 幂等语义：设备本来就不在册或已离线时**不广播**（避免重复 offline 刷屏）。
func (b *StateBus) MarkOffline(id string) {
	if id == "" {
		return
	}
	b.mu.Lock()
	prev, existed := b.states[id]
	if !existed || !prev.Online {
		b.mu.Unlock()
		return
	}
	prev.Online = false
	prev.LastSeenAt = b.now().UnixMilli()
	// 离线时清掉协议版本（它只描述「当前连接」，指望它有意义是一种误读）。
	prev.ProtocolVersion = 0
	b.states[id] = prev
	b.mu.Unlock()

	b.publish(DeviceStateEvent{
		Type:   StateEventDeviceOffline,
		Device: &prev,
		At:     prev.LastSeenAt,
	})
}

// Remove 移除设备并广播 device.revoked（= 解除绑定）。
//
// 与 MarkOffline 的关键区别：本方法是**终态**，前端收到后必须把设备从列表删除，
// 而不是标记为离线。因为它对应 devices.revoked_at 已写入的事实。
//
// 不在册也广播：管理端吊销时该设备可能从未建立过长连接（StateBus 里没有它的条目），
// 但**其他标签页/其他浏览器**的列表里可能有它（数据来自 GET /devices 而非 SSE 快照），
// 若因为「本地不在册」就不广播，那些页面就永远收不到消失通知。
// 故构造一个最小快照（仅 device_id）作为事件载荷。
func (b *StateBus) Remove(id string) {
	if id == "" {
		return
	}
	b.mu.Lock()
	prev, existed := b.states[id]
	delete(b.states, id)
	b.mu.Unlock()

	info := DeviceState{DeviceID: id}
	if existed {
		// 带上已知的展示字段，便于前端在移除前做一次「已解绑"设备名"」的提示。
		info.Name = prev.Name
		info.Kind = prev.Kind
		info.UserID = prev.UserID
		info.LastSeenAt = prev.LastSeenAt
	}
	b.publish(DeviceStateEvent{
		Type:   StateEventDeviceRevoked,
		Device: &info,
		At:     b.now().UnixMilli(),
	})
}

// Touch 仅刷新 LastSeenAt（心跳场景），**不广播**。
//
// 为什么心跳不广播：心跳间隔默认 15s，若每次心跳都推一次 SSE，
// 观察侧每秒都会被无意义的「什么都没变」刷屏。UI 需要的是「上线/离线」
// 这类**状态跃迁**，而不是连续时间戳。
func (b *StateBus) Touch(id string) {
	if id == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.states[id]
	if !ok {
		return
	}
	s.LastSeenAt = b.now().UnixMilli()
	b.states[id] = s
}

// Subscribe 订阅状态变化。
//
// 语义与 events.Bus.SubscribeAll 一致：**只推新变化，不补发历史**；
// 但为保证「新订阅者立刻知道当前全貌」，调用方应先读一次 Snapshot()
// 作为首帧（handler 层就是这么做的）。
func (b *StateBus) Subscribe() StateSubscription {
	b.mu.Lock()
	defer b.mu.Unlock()
	sub := &stateSubscriber{ch: make(chan DeviceStateEvent, b.bufferSize)}
	b.subs[sub] = struct{}{}
	return sub
}

// Unsubscribe 取消订阅（幂等）。
//
// 接受接口类型：允许调用方持有 StateSubscription 而无需知道具体实现。
func (b *StateBus) Unsubscribe(sub StateSubscription) {
	if sub == nil {
		return
	}
	s, ok := sub.(*stateSubscriber)
	if !ok {
		return
	}
	b.mu.Lock()
	delete(b.subs, s)
	b.mu.Unlock()
	s.close()
}

// C 事件通道（关闭表示被断开：慢消费者或总线关闭）。
func (s *stateSubscriber) C() <-chan DeviceStateEvent { return s.ch }

func (s *stateSubscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
}

// publish 非阻塞广播。
//
// 慢消费者处理：缓冲满即关闭该订阅并移除，**绝不阻塞发布方** ——
// 发布方是 WS 网关的 OnOnline/OnOffline 回调，阻塞它会让设备连接
// 建立/断开的处理卡住，进而拖垮整个网关。
func (b *StateBus) publish(ev DeviceStateEvent) {
	b.mu.Lock()
	var slow []*stateSubscriber
	for sub := range b.subs {
		sub.mu.Lock()
		if sub.closed {
			sub.mu.Unlock()
			slow = append(slow, sub)
			continue
		}
		select {
		case sub.ch <- ev:
			sub.mu.Unlock()
		default:
			// 缓冲满：标记为慢消费者，稍后统一移除。
			sub.closed = true
			close(sub.ch)
			sub.mu.Unlock()
			slow = append(slow, sub)
		}
	}
	for _, sub := range slow {
		delete(b.subs, sub)
	}
	b.mu.Unlock()
}

// SubscriberCount 当前订阅者数量（观测与测试用）。
func (b *StateBus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
