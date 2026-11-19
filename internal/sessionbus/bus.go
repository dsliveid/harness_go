// Package sessionbus 的「会话变更广播」：把会话的写入事件转成可订阅的变更流，
// 供 Web / 移动端 / 桌面端实时刷新**对话列表与状态**（`GET /api/v1/sessions/events`）。
//
// # 为什么独立成一套（不复用 events.Bus）
//
// events.Bus 是 **per-Run 序号总线**：`Publish(runID, type, payload)` 硬性要求
// runID 非空，事件结构含 RunID 与「Run 内单调递增的 Seq」。而会话列表变更：
//   - **没有 Run 归属**（重命名、归档、删除都与某个 Run 无关）；
//   - 语义是「当前态」而非事件流 —— 重连后只需拿最新列表，
//     不需要按 seq 补发历史（与 SSE 的 Last-Event-ID 语义正相反）。
//
// 这与 device.StateBus 的取舍完全一致（见 internal/device/state_bus.go 文件头），
// 只是观测对象从「设备」换成「会话」。
//
// # 设计口径：事件只带 ID，不带数据
//
// 本总线**刻意不携带会话内容**，只广播 `{type, session_id, at}`：
//   - 埋点极简：任何写入路径只需 `PublishUpdated(sid)`，不必先查出完整会话；
//   - 数据永不陈旧：SSE handler 收到事件后**回查 DB** 组装权威视图，
//     避免「事务未提交就广播导致客户端读到旧值」这类时序陷阱；
//   - 天然幂等：重复事件只表现为重复查询，不会污染客户端状态。
//
// 代价是每个订阅者各查一次库。会话列表查询按主键/索引，且在**节流合并**后
// 每次真实变更每端最多 1 次查询，代价可忽略。
//
// # 为什么必须节流合并
//
// `sessions.total_tokens` / `last_message_at` 会在**每条消息、每个工具结果**
// 落库时更新（store.AddTokensAndStatus / TouchMessage）。若每次直推，
// 一次 Run 会向所有端推送几十上百帧列表变更 —— 既是带宽浪费，
// 也会让前端列表无谓重排。故更新类事件走 `PublishUpdated`（默认 1s 合并窗口），
// 只有「Run 状态跃迁」这类低频且有实时性要求的事件走 `PublishImmediate`。
package sessionbus

import (
	"sync"
	"time"
)

// 事件类型常量。
const (
	// EventSnapshot 订阅首帧的全量快照（由 handler 查库组装，不经过本总线）。
	EventSnapshot = "snapshot"
	// EventUpdated 某会话的新增/改名/状态跃迁/计数变化（前端按 session_id upsert）。
	EventUpdated = "session.updated"
	// EventRemoved 某会话被真删除（前端应从列表移除）。
	//
	// 为什么归档也走 updated 而不是 removed：归档会话在「归档中心」依然可见，
	// 只是从活跃列表移出。是否移除由前端的列表分区规则决定，服务端只如实报
	// archived_at —— 若这里发 removed，前端会把归档误当成删除，归档中心也拿不到它。
	EventRemoved = "session.removed"
)

// 领域列表变更事件（**不带任何数据，纯信号**）。
//
// 为什么与会话事件共用同一条 SSE 连接而不新建端点：
//   三端已经为会话列表维持了一条长连接，再为项目/工作空间各开一条会让连接数
//   从 1 变 3（多标签页再放大），且每条都要独立维护续保/重连逻辑。
//
// 为什么只发信号、让前端整表重拉，而不是像会话那样推数据：
//   - 这两个列表变更频率极低（用户手点几次/天），整表重拉的开销可忽略；
//   - project 有**前端独有字段**（workspaceId 分组），服务端视图不足以独立重建
//     本地状态 —— 强行推全量反而会覆盖掉前端字段，制造新 bug；
//   - 复用前端既有的 loadProjects/loadWorkspaces，天然继承其合并语义。
//
// 代价是每次变更多一次列表请求。以本项目的数据规模（< 20 行）可忽略。
const (
	// EventProjectsChanged 项目列表发生了变化（增 / 改 / 删）。
	EventProjectsChanged = "projects.changed"
	// EventWorkspacesChanged 工作空间列表发生了变化（增 / 改 / 删）。
	EventWorkspacesChanged = "workspaces.changed"
)

// Event 一条会话变更通知（**不含会话内容**，见包注释「事件只带 ID」）。
type Event struct {
	Type string `json:"type"`
	// SessionID 变更的会话 id。
	SessionID string `json:"session_id"`
	// At 事件发生时刻（Unix 毫秒）。
	At int64 `json:"at"`
}

// Subscription 变更订阅句柄（跨包可见）。
//
// 导出一层接口的原因与 device.StateSubscription 相同：Bus 在本包，
// SSE handler 在 api 包，若 Subscribe 返回未导出类型，api 包无法持有。
type Subscription interface {
	// C 事件通道（关闭表示被断开：慢消费者）。
	C() <-chan Event
}

// subscriber 单个订阅者。
type subscriber struct {
	ch     chan Event
	closed bool
	mu     sync.Mutex
}

func (s *subscriber) C() <-chan Event { return s.ch }

func (s *subscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
}

// Options 构造参数（全部可省略）。
type Options struct {
	// BufferSize 单订阅者事件缓冲（默认 32）。设小是为了让慢消费者尽快被
	// 识别并断开，而不是无限积压内存。
	BufferSize int
	// MergeWindow 更新类事件的合并窗口（默认 1 秒）。<=0 表示不合并（测试用）。
	MergeWindow time.Duration
	// Now 可注入时钟（测试用）。
	Now func() time.Time
}

// Bus 会话变更广播总线。并发安全。
//
// 慢消费者策略与 device.StateBus / events.Bus 一致：**缓冲满即丢弃该订阅**，
// 绝不阻塞发布方（发布方是 HTTP handler 的写路径，阻塞它们会拖慢整个 API）。
type Bus struct {
	mu    sync.Mutex
	subs  map[*subscriber]struct{}
	// pending 处于合并窗口内的会话 id 集合。窗口结束时统一广播一次。
	//
	// 只记录「有没有待发事件」而不记录事件内容，因为 handler 收到事件后会回查 DB
	// 拿最新状态 —— 内容天然是最新的，无需在总线里合并字段。
	pending     map[string]struct{}
	now         func() time.Time
	bufferSize  int
	mergeWindow time.Duration
}

// New 构造会话变更总线。
func New(opts Options) *Bus {
	size := opts.BufferSize
	if size <= 0 {
		size = 32
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	window := opts.MergeWindow
	if window == 0 {
		window = time.Second
	}
	if window < 0 {
		window = 0
	}
	return &Bus{
		subs:        make(map[*subscriber]struct{}),
		pending:     make(map[string]struct{}),
		now:         now,
		bufferSize:  size,
		mergeWindow: window,
	}
}

// PublishUpdated 广播「某会话可能变化了」（走合并窗口）。
//
// 幂等：窗口内对同一会话的重复调用只会在窗口结束时产生**一帧**事件。
// 因此高频调用方（每条消息落库都调）无需自行去抖。
func (b *Bus) PublishUpdated(sessionID string) {
	if sessionID == "" {
		return
	}
	b.mu.Lock()
	if _, ok := b.pending[sessionID]; ok {
		// 已在窗口内：无需重置计时器。窗口结束时 handler 会回查 DB，
		// 拿到的必然是此刻最新状态，所以「不重置」不会丢失后续变更。
		b.mu.Unlock()
		return
	}
	b.pending[sessionID] = struct{}{}
	window := b.mergeWindow
	b.mu.Unlock()

	if window <= 0 {
		b.flush(sessionID)
		return
	}
	time.AfterFunc(window, func() { b.flush(sessionID) })
}

// PublishImmediate 立即广播一条事件（不合并）。
//
// 用于 Run 状态跃迁这类**低频且要求低延迟**的场景（pending→running→终态），
// 以及删除（必须立刻让各端把会话摘掉，合并窗口只会让人以为删失败了）。
func (b *Bus) PublishImmediate(sessionID, typ string) {
	if sessionID == "" || typ == "" {
		return
	}
	b.broadcast(Event{Type: typ, SessionID: sessionID, At: b.now().UnixMilli()})
}

// PublishRemoved 广播会话删除（立即），并撤销该会话尚在窗口内的更新事件。
//
// 撤销的必要性：若先 PublishUpdated 再删除，窗口结束时的那帧 updated 会让
// 客户端回查一个已不存在的会话（handler 查到 not found 会静默跳过，虽不出错，
// 但属于无意义的穿仓查询）。这里直接清掉更干净。
func (b *Bus) PublishRemoved(sessionID string) {
	if sessionID == "" {
		return
	}
	b.mu.Lock()
	delete(b.pending, sessionID)
	b.mu.Unlock()
	b.broadcast(Event{Type: EventRemoved, SessionID: sessionID, At: b.now().UnixMilli()})
}

// PublishDomain 广播一条「领域列表变更」信号（不含任何数据）。
//
// 与 PublishImmediate 的区别：领域事件没有 sessionID，也不回查 DB
// （它只是「去重拉一次列表」的提示）。故走独立入口，避免调用方误传 sessionID
// 污染会话事件语义。
//
// 为什么用立即广播而非合并窗口：项目/工作空间的增删改都是**显式用户操作**
// （一次点击一帧），不存在 total_tokens 那种高频写，无需节流。
func (b *Bus) PublishDomain(typ string) {
	if typ == "" {
		return
	}
	b.broadcast(Event{Type: typ, At: b.now().UnixMilli()})
}

// Subscribe 订阅变更。
//
// 语义与 device.StateBus 一致：**只推新变化，不补发历史**；
// 为保证新订阅者立刻知道当前全貌，调用方应先查库拿到列表作为首帧
// （SSE handler 就是这么做的：先 Subscribe 再查库）。
func (b *Bus) Subscribe() Subscription {
	b.mu.Lock()
	defer b.mu.Unlock()
	sub := &subscriber{ch: make(chan Event, b.bufferSize)}
	b.subs[sub] = struct{}{}
	return sub
}

// Unsubscribe 取消订阅（幂等）。
func (b *Bus) Unsubscribe(sub Subscription) {
	if sub == nil {
		return
	}
	s, ok := sub.(*subscriber)
	if !ok {
		return
	}
	b.mu.Lock()
	delete(b.subs, s)
	b.mu.Unlock()
	s.close()
}

// SubscriberCount 当前订阅者数量（观测与测试用）。
func (b *Bus) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// flush 结算一帧合并窗口事件。
func (b *Bus) flush(sessionID string) {
	b.mu.Lock()
	_, ok := b.pending[sessionID]
	delete(b.pending, sessionID)
	b.mu.Unlock()
	if !ok {
		// 已被 PublishRemoved 撤销：不发更新帧。
		return
	}
	b.broadcast(Event{Type: EventUpdated, SessionID: sessionID, At: b.now().UnixMilli()})
}

// broadcast 非阻塞广播。
//
// 慢消费者：缓冲满即关闭该订阅并移除，**绝不阻塞发布方** ——
// 发布方是 HTTP 写路径，阻塞它等于把一次列表刷新拖成整个 API 卡顿。
func (b *Bus) broadcast(ev Event) {
	b.mu.Lock()
	var slow []*subscriber
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
