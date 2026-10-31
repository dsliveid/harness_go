package device

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// ============ 在线连接注册表 + 心跳超时判定（阶段 C-3） ============
//
// 职责边界：
//   - 本文件**只管在线状态**：谁在线、谁的连接是当前有效连接、谁已超时该判离线
//   - **不碰 DB**：落库通过 HubOptions 的三组回调交给 service 层（同库、无跨库事务）
//   - 线程安全：所有导出方法可并发调用；单连接只允许一个读循环 + 一个写循环

// Connection 网关连接的最小契约（hub 只关心这几项，便于测试注入假连接）。
type Connection interface {
	// DeviceID 该连接所属设备 ID。
	DeviceID() string
	// Send 线程安全地写出一个信封（内部串行化写）。
	Send(env Envelope) error
	// CloseWith 以指定错误码关闭连接（幂等）。
	CloseWith(code, reason string)
	// LastSeen 最近一次收到端侧帧的时间（心跳超时判定依据）。
	LastSeen() time.Time
	// RemoteAddr 端侧地址（仅日志）。
	RemoteAddr() string
}

// HubOptions Hub 依赖（回调为持久化钩子，nil 时降级为纯内存态，便于单测）。
type HubOptions struct {
	Log *slog.Logger
	// OfflineAfter 无帧判离线阈值（默认 45s = 3×15s 心跳）。
	OfflineAfter time.Duration
	// SweepInterval 巡检间隔（默认 OfflineAfter/3）。
	SweepInterval time.Duration

	// OnOnline 设备上线（service 注入 Devices().MarkOnline）。
	OnOnline func(ctx context.Context, deviceID string)
	// OnHeartbeat 收到 ping（service 注入 Devices().UpdateHeartbeat）。
	OnHeartbeat func(ctx context.Context, deviceID string, at int64, load json.RawMessage, capsHash string, proto int)
	// OnOffline 设备离线（service 注入 Devices().MarkOffline）。
	OnOffline func(ctx context.Context, deviceID string)
}

// Hub 在线连接注册表。
//
// 关键语义（后续 C-4/C-6 直接复用，勿重新解释）：
//   - 一台设备同时只有**一条有效连接**；新连接注册会使旧连接收到 device.replaced 并被关闭
//   - Unregister 只在「当前连接仍是自己」时生效，避免旧连接的清理逻辑误删新连接
//   - 心跳超时 = now - LastSeen > OfflineAfter → 关连接 + 判离线 + 回调落库
type Hub struct {
	log           *slog.Logger
	offlineAfter  time.Duration
	sweepInterval time.Duration

	onOnline    func(ctx context.Context, deviceID string)
	onHeartbeat func(ctx context.Context, deviceID string, at int64, load json.RawMessage, capsHash string, proto int)
	onOffline   func(ctx context.Context, deviceID string)

	now func() time.Time

	mu     sync.RWMutex
	conns  map[string]Connection
	closed bool

	// onlineCount 便于无锁读取在线数（/devices 与 pong.pending 参考）。
	onlineCount atomic.Int64

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewHub 创建注册表（不自动启动巡检，需显式 Start）。
func NewHub(opts HubOptions) *Hub {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	offline := opts.OfflineAfter
	if offline <= 0 {
		offline = 45 * time.Second
	}
	interval := opts.SweepInterval
	if interval <= 0 {
		interval = offline / 3
		if interval < time.Second {
			interval = time.Second
		}
	}
	return &Hub{
		log:           log,
		offlineAfter:  offline,
		sweepInterval: interval,
		onOnline:      opts.OnOnline,
		onHeartbeat:   opts.OnHeartbeat,
		onOffline:     opts.OnOffline,
		now:           time.Now,
		conns:         make(map[string]Connection),
	}
}

// OfflineAfter 无帧判离线阈值。
func (h *Hub) OfflineAfter() time.Duration { return h.offlineAfter }

// SweepInterval 巡检间隔。
func (h *Hub) SweepInterval() time.Duration { return h.sweepInterval }

// SetClock 注入时钟（测试用）。
func (h *Hub) SetClock(fn func() time.Time) {
	if fn != nil {
		h.now = fn
	}
}

// Start 启动心跳巡检协程（重复调用安全；ctx 结束自动停）。
func (h *Hub) Start(ctx context.Context) {
	h.mu.Lock()
	if h.closed || h.cancel != nil {
		h.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	h.cancel = cancel
	h.mu.Unlock()

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ticker := time.NewTicker(h.sweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if n := h.Sweep(); n > 0 {
					h.log.Warn("设备心跳超时被判离线", "count", n, "offline_after_s", int(h.offlineAfter.Seconds()))
				}
			}
		}
	}()
}

// Register 注册连接（同设备旧连接会被关闭并返回 replaced=true）。
func (h *Hub) Register(c Connection) (replaced bool) {
	id := c.DeviceID()
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		c.CloseWith(FrameCodeServerClosing, "服务端正在关闭")
		return false
	}
	old, exists := h.conns[id]
	h.conns[id] = c
	h.onlineCount.Store(int64(len(h.conns)))
	h.mu.Unlock()

	if exists && old != nil {
		// 旧连接必须主动断开：否则双连接并发写同一设备的端侧状态
		old.CloseWith(FrameCodeReplaced, "同设备新连接已接管")
		replaced = true
	}
	if h.onOnline != nil {
		h.call(h.onOnline, id)
	}
	return replaced
}

// Unregister 注销连接；只有当前连接仍是 c 时才删除（返回是否删除成功）。
func (h *Hub) Unregister(c Connection) bool {
	id := c.DeviceID()
	h.mu.Lock()
	cur, ok := h.conns[id]
	if !ok || cur != c {
		h.mu.Unlock()
		return false
	}
	delete(h.conns, id)
	h.onlineCount.Store(int64(len(h.conns)))
	h.mu.Unlock()

	if h.onOffline != nil {
		h.call(h.onOffline, id)
	}
	return true
}

// Get 取当前有效连接。
func (h *Hub) Get(deviceID string) (Connection, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.conns[deviceID]
	return c, ok
}

// Online 设备当前是否在线（有有效连接）。
func (h *Hub) Online(deviceID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.conns[deviceID]
	return ok
}

// Count 在线连接数（设备数，非连接数）。
func (h *Hub) Count() int { return int(h.onlineCount.Load()) }

// IDs 当前在线设备 ID 列表（顺序不保证，调用方需自行排序）。
func (h *Hub) IDs() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ids := make([]string, 0, len(h.conns))
	for id := range h.conns {
		ids = append(ids, id)
	}
	return ids
}

// Beat 记录心跳并落库（网关收到 ping 时调用）。
func (h *Hub) Beat(deviceID string, load json.RawMessage, capsHash string, proto int) {
	if h.onHeartbeat == nil {
		return
	}
	h.call(func(ctx context.Context, id string) {
		h.onHeartbeat(ctx, id, h.now().UnixMilli(), load, capsHash, proto)
	}, deviceID)
}

// Sweep 巡检一次：关闭超过 OfflineAfter 无帧的连接并判离线；返回判定离线数。
func (h *Hub) Sweep() int {
	now := h.now()
	type victim struct {
		conn Connection
		idle time.Duration
	}
	var victims []victim

	h.mu.Lock()
	for id, c := range h.conns {
		idle := now.Sub(c.LastSeen())
		if idle <= h.offlineAfter {
			continue
		}
		delete(h.conns, id)
		victims = append(victims, victim{conn: c, idle: idle})
	}
	h.onlineCount.Store(int64(len(h.conns)))
	h.mu.Unlock()

	for _, v := range victims {
		h.log.Warn("设备心跳超时",
			"device_id", v.conn.DeviceID(),
			"idle_s", int(v.idle.Seconds()),
			"remote", v.conn.RemoteAddr())
		v.conn.CloseWith(FrameCodeOfflineTimeout, "心跳超时（超过 offline_after_s 无帧）")
		if h.onOffline != nil {
			h.call(h.onOffline, v.conn.DeviceID())
		}
	}
	return len(victims)
}

// CloseDevice 主动断开指定设备（实现 service.DeviceDisconnector：吊销设备时用）。
//
// 语义：仅关闭连接，**不**回调 OnOffline（吊销由 service 自己落库为 revoked 终态，
// 这里再写 offline 会覆盖终态）。
func (h *Hub) CloseDevice(deviceID, reason string) {
	if reason == "" {
		reason = "device.revoked"
	}
	h.mu.Lock()
	c, ok := h.conns[deviceID]
	if ok {
		delete(h.conns, deviceID)
		h.onlineCount.Store(int64(len(h.conns)))
	}
	h.mu.Unlock()
	if !ok || c == nil {
		return
	}
	// 吊销与替换用不同协议码，端侧据此决定「提示重新配对」还是「静默重连」
	code := FrameCodeRevoked
	if reason == FrameCodeReplaced {
		code = FrameCodeReplaced
	}
	c.CloseWith(code, reason)
	h.log.Info("已断开设备长连接", "device_id", deviceID, "reason", reason)
}

// CloseAll 关闭全部连接并停止巡检（优雅关闭用；幂等）。
func (h *Hub) CloseAll(reason string) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	cancel := h.cancel
	conns := make([]Connection, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	h.conns = make(map[string]Connection)
	h.onlineCount.Store(0)
	h.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	h.wg.Wait()

	if reason == "" {
		reason = "服务端关闭"
	}
	for _, c := range conns {
		c.CloseWith(FrameCodeServerClosing, reason)
	}
}

// call 以独立短超时 ctx 调用落库回调（回调不可阻塞读循环/巡检）。
func (h *Hub) call(fn func(ctx context.Context, deviceID string), deviceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	fn(ctx, deviceID)
}
