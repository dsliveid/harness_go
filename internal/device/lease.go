package device

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"harness-go/internal/model"
)

// ============ 会话租约（阶段 C-6：跨端单写者） ============
//
// 职责边界：
//   - 本文件只做「租约语义 + 抢占通知」，**落库经注入的窄接口**（LeaseStore）
//   - 与本地 mutex 的关系：**两者并存**。本地 mutex 解决「进程内同会话 Run 串行」；
//     租约解决「跨端可见 + 抢占语义（epoch+1）+ 旧端收到 lease.revoked」
//   - 绝不承认端侧本地 seq：写路径一律服务端分配（不变量 1）
//
// epoch 语义（复用 store.LeaseRepo 的原子实现，勿在此重复判定）：
//   - 首次建立租约 → epoch = 1
//   - 同一持有者且未过期 → 纯续期，epoch 不变（Renew 即此路径）
//   - 换持有者 / 已过期后接管 → epoch = 旧 epoch + 1，并向旧端推 lease.revoked

// DefaultLeaseTTL 租约默认有效期。
//
// 取值考虑：需覆盖一次 Run 的最长常态时长（模型多轮 + 工具调用），
// 同时保证端侧异常消失后租约能自动收敛（引擎每步 Renew 续期）。
const DefaultLeaseTTL = 5 * time.Minute

// 租约变更原因（写入 lease.revoked.payload.reason，端侧据此提示措辞）。
const (
	// LeaseRevokedSuperseded 被其他执行端抢占（epoch+1）。
	LeaseRevokedSuperseded = "superseded"
	// LeaseRevokedExpired 持有者租约过期后被接管。
	LeaseRevokedExpired = "expired"
	// LeaseRevokedReleased 持有者主动释放（服务端通知其它关注端时用）。
	LeaseRevokedReleased = "released"
)

// LeaseStore 会话租约仓储窄接口（在 device 包内定义，store.LeaseRepo 实现其方法集）。
//
// 语义约定（由实现方保证原子性）：
//   - Acquire 返回 (生效后的租约, 是否发生写者变更, error)；
//     变更 = 换了持有者（需要向旧端推 revoked），同持有者续期返回 false
//   - deviceID 为空串表示**服务端本地执行面**持有（与 model.SessionLease 一致）
type LeaseStore interface {
	Acquire(ctx context.Context, sessionID, deviceID string, now, ttlMillis int64) (model.SessionLease, bool, error)
	Get(ctx context.Context, sessionID string) (*model.SessionLease, error)
	Release(ctx context.Context, sessionID, deviceID string) (bool, error)
	DeleteExpired(ctx context.Context, now int64) (int64, error)
}

// ConnFinder 在线连接查找（*Hub 满足本接口；nil 时退化为「不推送」）。
type ConnFinder interface {
	Get(deviceID string) (Connection, bool)
}

// LeaseManagerOptions LeaseManager 依赖。
type LeaseManagerOptions struct {
	Log *slog.Logger
	// Store 租约仓储（必填）。
	Store LeaseStore
	// Hub 在线连接查找（可为 nil：无连接则只更新 DB，不推送）。
	Hub ConnFinder
	// TTL 租约有效期（<=0 回退 DefaultLeaseTTL）。
	TTL time.Duration
	// Now 时钟注入（测试用；缺省 time.Now）。
	Now func() time.Time
}

// LeaseManager 会话租约管理器（实现 epoch+1 抢占与 lease.revoked 推送）。
type LeaseManager struct {
	log   *slog.Logger
	store LeaseStore
	ttl   time.Duration
	now   func() time.Time

	// hub 后置注入（SetHub）：LeaseManager 可能在 WS 网关之前构造，
	// 而推送 lease.revoked 需要在线连接表，故用可写指针 + 读写锁消除竞态。
	mu  sync.RWMutex
	hub ConnFinder
}

// ErrLeaseStoreMissing 未注入租约仓储（装配期错误，不是运行时业务错误）。
var ErrLeaseStoreMissing = errors.New("device: 未注入租约仓储")

// NewLeaseManager 创建租约管理器。
func NewLeaseManager(opts LeaseManagerOptions) *LeaseManager {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &LeaseManager{log: log, store: opts.Store, hub: opts.Hub, ttl: ttl, now: now}
}

// TTL 租约有效期（端点回显与断言用）。
func (m *LeaseManager) TTL() time.Duration { return m.ttl }

// SetHub 后置注入在线连接查找（装配期：LeaseManager 先建、Hub 后建时用）。
func (m *LeaseManager) SetHub(h ConnFinder) {
	m.mu.Lock()
	m.hub = h
	m.mu.Unlock()
}

// connFinder 取当前在线连接查找（可能为 nil）。
func (m *LeaseManager) connFinder() ConnFinder {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.hub
}

// SetClock 注入时钟（测试用）。
func (m *LeaseManager) SetClock(fn func() time.Time) {
	if fn != nil {
		m.now = fn
	}
}

// Acquire 抢占或续期租约，返回生效后的租约。
//
// holder 为空串表示服务端本地执行面持有。**不会因抢占失败而报错**：
// 抢占本身就是语义（epoch+1），旧持有者通过 lease.revoked 得知。
//
// 实现顺序：先 best-effort 读旧持有者（仅用于通知，失败不影响正确性，
// 权威判定在 store 的原子事务内完成）→ 原子 Acquire → 变更则推送 revoked。
func (m *LeaseManager) Acquire(ctx context.Context, sessionID, holder string) (model.SessionLease, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return model.SessionLease{}, errors.New("device: 租约缺少 session_id")
	}
	if m.store == nil {
		return model.SessionLease{}, ErrLeaseStoreMissing
	}

	prev, _ := m.store.Get(ctx, sessionID)
	now := m.now().UnixMilli()

	lease, changed, err := m.store.Acquire(ctx, sessionID, holder, now, m.ttl.Milliseconds())
	if err != nil {
		return model.SessionLease{}, err
	}

	if changed && prev != nil && prev.DeviceID != "" && prev.DeviceID != holder {
		reason := LeaseRevokedSuperseded
		if prev.Expired(now) {
			reason = LeaseRevokedExpired
		}
		m.pushRevoked(ctx, prev.DeviceID, lease, reason)
	}
	return lease, nil
}

// Renew 续期租约（同持有者且未过期 → epoch 不变）。
//
// 注意：若租约已过期，仓储语义为「过期后接管」→ epoch+1 并通知旧端，
// 这样端侧长时间掉线回来也不会静默并列成为写者。引擎在每一步迭代调用本方法。
func (m *LeaseManager) Renew(ctx context.Context, sessionID, holder string) error {
	_, err := m.Acquire(ctx, sessionID, holder)
	return err
}

// Release 释放租约（仅持有者本人生效；幂等）。
func (m *LeaseManager) Release(ctx context.Context, sessionID, holder string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("device: 租约缺少 session_id")
	}
	if m.store == nil {
		return ErrLeaseStoreMissing
	}
	released, err := m.store.Release(ctx, sessionID, holder)
	if err != nil {
		return err
	}
	if released {
		m.log.Debug("会话租约已释放", "session_id", sessionID, "holder", holder)
	}
	return nil
}

// Get 查询会话租约。
//
// 约定（由 LeaseStore 适配方保证）：**不存在返回 (nil, nil)**，语义是「当前无写者」
// 而非错误；真实读故障仍返回 error。这样 device 包既不需要 import store 判定
// ErrNotFound，也不会把基础设施故障伪装成「无租约」。
func (m *LeaseManager) Get(ctx context.Context, sessionID string) (*model.SessionLease, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, errors.New("device: 租约缺少 session_id")
	}
	if m.store == nil {
		return nil, ErrLeaseStoreMissing
	}
	return m.store.Get(ctx, sessionID)
}

// Active 租约在 now 时刻是否仍有效（nil = 无租约 = 空闲）。
func Active(lease *model.SessionLease, now int64) bool {
	return lease != nil && !lease.Expired(now)
}

// PurgeExpired 清理过期租约（维护任务/启动巡检可调用），返回删除行数。
func (m *LeaseManager) PurgeExpired(ctx context.Context) (int64, error) {
	if m.store == nil {
		return 0, ErrLeaseStoreMissing
	}
	return m.store.DeleteExpired(ctx, m.now().UnixMilli())
}

// pushRevoked 向旧持有者推送 lease.revoked（连接不在线时静默跳过）。
//
// 铁律：通知是**尽力而为**——推送失败绝不影响租约本身已生效的事实
// （DB 才是权威；端侧下一次操作会被 epoch 校验拒绝）。
func (m *LeaseManager) pushRevoked(ctx context.Context, deviceID string, lease model.SessionLease, reason string) {
	hub := m.connFinder()
	if hub == nil || deviceID == "" {
		return
	}
	conn, ok := hub.Get(deviceID)
	if !ok || conn == nil {
		m.log.Debug("旧持有者不在线，跳过 lease.revoked 推送",
			"device_id", deviceID, "session_id", lease.SessionID, "epoch", lease.Epoch)
		return
	}
	env := MustEnvelope(MsgLeaseRevoked, "", LeaseRevokedPayload{
		SessionID: lease.SessionID,
		Epoch:     lease.Epoch,
		Reason:    reason,
	})
	if err := conn.Send(env); err != nil {
		m.log.Warn("推送 lease.revoked 失败（租约已生效，仅通知未达）",
			"device_id", deviceID, "session_id", lease.SessionID, "err", err)
		return
	}
	m.log.Info("已通知旧持有者租约被抢占",
		"device_id", deviceID, "session_id", lease.SessionID,
		"epoch", lease.Epoch, "reason", reason, "ctx_done", ctx.Err() != nil)
}
