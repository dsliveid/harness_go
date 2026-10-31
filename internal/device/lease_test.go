package device

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"harness-go/internal/model"
)

// ============ 会话租约（阶段 C-6）单测 ============

// fakeLeaseStore 内存租约仓储，复刻 store.LeaseRepo 的 epoch 语义。
type fakeLeaseStore struct {
	mu  sync.Mutex
	row map[string]*model.SessionLease
	// failAcquire 注入 Acquire 故障（断言「租约失败不阻断 Run」）。
	failAcquire error
	// failGet 注入 Get 故障。
	failGet error
}

func newFakeLeaseStore() *fakeLeaseStore {
	return &fakeLeaseStore{row: make(map[string]*model.SessionLease)}
}

func (s *fakeLeaseStore) Acquire(_ context.Context, sessionID, deviceID string,
	now, ttlMillis int64) (model.SessionLease, bool, error) {
	if s.failAcquire != nil {
		return model.SessionLease{}, false, s.failAcquire
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.row[sessionID]
	if !ok {
		l := &model.SessionLease{
			SessionID: sessionID, DeviceID: deviceID, Epoch: 1,
			ExpiresAt: now + ttlMillis, HeartbeatAt: now, CreatedAt: now,
		}
		s.row[sessionID] = l
		return *l, deviceID != "", nil
	}
	epoch := cur.Epoch
	changed := false
	if cur.DeviceID != deviceID || cur.ExpiresAt <= now {
		epoch = cur.Epoch + 1
		changed = cur.DeviceID != deviceID
	}
	cur.DeviceID = deviceID
	cur.Epoch = epoch
	cur.ExpiresAt = now + ttlMillis
	cur.HeartbeatAt = now
	return *cur, changed, nil
}

func (s *fakeLeaseStore) Get(_ context.Context, sessionID string) (*model.SessionLease, error) {
	if s.failGet != nil {
		return nil, s.failGet
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.row[sessionID]
	if !ok {
		return nil, nil // 约定：不存在 = (nil, nil)
	}
	cp := *l
	return &cp, nil
}

func (s *fakeLeaseStore) Release(_ context.Context, sessionID, deviceID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.row[sessionID]
	if !ok || l.DeviceID != deviceID {
		return false, nil
	}
	delete(s.row, sessionID)
	return true, nil
}

func (s *fakeLeaseStore) DeleteExpired(_ context.Context, now int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for k, v := range s.row {
		if v.ExpiresAt <= now {
			delete(s.row, k)
			n++
		}
	}
	return n, nil
}

// fakeConnFinder 在线连接表：按 deviceID 返回记录信封的假连接。
type leaseFakeConn struct {
	id   string
	mu   sync.Mutex
	sent []Envelope
	fail error
}

func (c *leaseFakeConn) DeviceID() string { return c.id }
func (c *leaseFakeConn) Send(env Envelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		return c.fail
	}
	c.sent = append(c.sent, env)
	return nil
}
func (c *leaseFakeConn) CloseWith(string, string) {}
func (c *leaseFakeConn) LastSeen() time.Time      { return time.Now() }
func (c *leaseFakeConn) RemoteAddr() string       { return "127.0.0.1:1" }

func (c *leaseFakeConn) envelopes() []Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Envelope, len(c.sent))
	copy(out, c.sent)
	return out
}

type fakeConnFinder struct {
	mu    sync.Mutex
	conns map[string]Connection
}

func newFakeConnFinder() *fakeConnFinder {
	return &fakeConnFinder{conns: make(map[string]Connection)}
}

func (f *fakeConnFinder) Get(deviceID string) (Connection, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.conns[deviceID]
	return c, ok
}

func (f *fakeConnFinder) put(c Connection) {
	f.mu.Lock()
	f.conns[c.DeviceID()] = c
	f.mu.Unlock()
}

// newTestLeaseManager 构造带可控时钟的租约管理器。
func newTestLeaseManager(t *testing.T, store *fakeLeaseStore, hub ConnFinder) (*LeaseManager, *time.Time) {
	t.Helper()
	now := time.UnixMilli(1_700_000_000_000)
	m := NewLeaseManager(LeaseManagerOptions{Store: store, Hub: hub, TTL: time.Minute})
	m.SetClock(func() time.Time { return now })
	return m, &now
}

// 首次建立租约 → epoch=1；同持有者续期 epoch 不变；换人抢占 epoch+1 且推送 lease.revoked。
func TestLeaseAcquireEpochAndRevokedPush(t *testing.T) {
	ctx := context.Background()
	store := newFakeLeaseStore()
	finder := newFakeConnFinder()
	m, now := newTestLeaseManager(t, store, finder)

	// ① 服务端本地首次持有：epoch=1，无旧持有者 → 不推送
	l1, err := m.Acquire(ctx, "sess-1", "")
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	if l1.Epoch != 1 || l1.DeviceID != "" {
		t.Fatalf("首次租约应为 epoch=1 / 服务端持有: %+v", l1)
	}
	if !Active(&l1, now.UnixMilli()) {
		t.Fatal("新建租约应处于有效状态")
	}

	// ② 同持有者续期：epoch 不变
	*now = now.Add(10 * time.Second)
	l2, err := m.Acquire(ctx, "sess-1", "")
	if err != nil {
		t.Fatalf("续期失败: %v", err)
	}
	if l2.Epoch != 1 {
		t.Fatalf("同持有者续期不应涨 epoch，得到 %d", l2.Epoch)
	}

	// ③ 端侧抢占服务端（有写入 lease.revoked 的需求，但服务端无连接 → 静默跳过）
	dev1 := &leaseFakeConn{id: "dev-1"}
	finder.put(dev1)
	*now = now.Add(10 * time.Second)
	l3, err := m.Acquire(ctx, "sess-1", "dev-1")
	if err != nil {
		t.Fatalf("设备抢占失败: %v", err)
	}
	if l3.Epoch != 2 || l3.DeviceID != "dev-1" {
		t.Fatalf("换持有者应 epoch=2 且 device_id=dev-1: %+v", l3)
	}
	if got := len(dev1.envelopes()); got != 0 {
		t.Fatalf("新持有者本身不应收到 revoked，得到 %d 帧", got)
	}

	// ④ 第二台设备抢占 dev-1 → dev-1 必须收到 lease.revoked（含新 epoch 与原因）
	dev2 := &leaseFakeConn{id: "dev-2"}
	finder.put(dev2)
	*now = now.Add(10 * time.Second)
	l4, err := m.Acquire(ctx, "sess-1", "dev-2")
	if err != nil {
		t.Fatalf("dev-2 抢占失败: %v", err)
	}
	if l4.Epoch != 3 {
		t.Fatalf("再次换人应 epoch=3，得到 %d", l4.Epoch)
	}
	sent := dev1.envelopes()
	if len(sent) != 1 {
		t.Fatalf("dev-1 应收到 1 帧 lease.revoked，得到 %d", len(sent))
	}
	if sent[0].Type != MsgLeaseRevoked {
		t.Fatalf("帧类型应为 %s，得到 %s", MsgLeaseRevoked, sent[0].Type)
	}
	if sent[0].V != CurrentProtocolVersion {
		t.Fatalf("帧版本应为 %d，得到 %d", CurrentProtocolVersion, sent[0].V)
	}
	var payload LeaseRevokedPayload
	if err := sent[0].Decode(&payload); err != nil {
		t.Fatalf("解析 lease.revoked 负载失败: %v", err)
	}
	if payload.SessionID != "sess-1" || payload.Epoch != 3 || payload.Reason != LeaseRevokedSuperseded {
		t.Fatalf("lease.revoked 负载错误: %+v", payload)
	}
}

// 租约过期后由同一设备接管：epoch+1（不允许静默续期，防止掉线端并列成为写者）。
func TestLeaseExpiredTakeoverBumpsEpoch(t *testing.T) {
	ctx := context.Background()
	store := newFakeLeaseStore()
	finder := newFakeConnFinder()
	m, now := newTestLeaseManager(t, store, finder)
	conn := &leaseFakeConn{id: "dev-9"}
	finder.put(conn)

	first, err := m.Acquire(ctx, "sess-x", "dev-9")
	if err != nil {
		t.Fatal(err)
	}
	if first.Epoch != 1 {
		t.Fatalf("首次应 epoch=1，得到 %d", first.Epoch)
	}

	// 推进到超过 TTL 后同设备再 Acquire → 过期接管，epoch+1
	*now = now.Add(2 * time.Minute)
	second, err := m.Acquire(ctx, "sess-x", "dev-9")
	if err != nil {
		t.Fatal(err)
	}
	if second.Epoch != 2 {
		t.Fatalf("过期后接管应 epoch=2，得到 %d", second.Epoch)
	}
	// 同设备不产生「写者变更」，故不推送 revoked（避免自我通知噪音）
	if got := len(conn.envelopes()); got != 0 {
		t.Fatalf("同持有者过期接管不应推送 revoked，得到 %d 帧", got)
	}
}

// Release 只对持有者本人生效，且幂等。
func TestLeaseReleaseOnlyHolder(t *testing.T) {
	ctx := context.Background()
	store := newFakeLeaseStore()
	m, _ := newTestLeaseManager(t, store, nil)

	if _, err := m.Acquire(ctx, "sess-r", "dev-a"); err != nil {
		t.Fatal(err)
	}
	// 非持有者释放：无效果
	if err := m.Release(ctx, "sess-r", "dev-b"); err != nil {
		t.Fatalf("非持有者释放不应报错: %v", err)
	}
	if lease, err := m.Get(ctx, "sess-r"); err != nil || lease == nil {
		t.Fatalf("非持有者释放后租约应仍在: lease=%+v err=%v", lease, err)
	}
	// 持有者释放成功，再释放幂等
	for i := 0; i < 2; i++ {
		if err := m.Release(ctx, "sess-r", "dev-a"); err != nil {
			t.Fatalf("第 %d 次释放失败: %v", i+1, err)
		}
	}
	if lease, err := m.Get(ctx, "sess-r"); err != nil || lease != nil {
		t.Fatalf("释放后应无租约: lease=%+v err=%v", lease, err)
	}
}

// Renew 走 Acquire 路径：同持有者不改 epoch；nil/空 sessionID 报错。
func TestLeaseRenewAndValidation(t *testing.T) {
	ctx := context.Background()
	store := newFakeLeaseStore()
	m, now := newTestLeaseManager(t, store, nil)

	if _, err := m.Acquire(ctx, "sess-n", "dev-n"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(30 * time.Second)
	if err := m.Renew(ctx, "sess-n", "dev-n"); err != nil {
		t.Fatalf("Renew 失败: %v", err)
	}
	lease, err := m.Get(ctx, "sess-n")
	if err != nil || lease == nil {
		t.Fatalf("Get 失败: lease=%+v err=%v", lease, err)
	}
	if lease.Epoch != 1 {
		t.Fatalf("Renew 不应涨 epoch，得到 %d", lease.Epoch)
	}
	if lease.ExpiresAt != now.Add(time.Minute).UnixMilli() {
		t.Fatalf("Renew 应刷新 expires_at，得到 %d", lease.ExpiresAt)
	}

	if _, err := m.Acquire(ctx, "  ", "dev-n"); err == nil {
		t.Fatal("空 session_id 应报错")
	}
	if _, err := m.Get(ctx, ""); err == nil {
		t.Fatal("空 session_id 的 Get 应报错")
	}
}

// 未注入仓储时所有方法返回装配期错误（不 panic，不静默成功）。
func TestLeaseManagerWithoutStore(t *testing.T) {
	ctx := context.Background()
	m := NewLeaseManager(LeaseManagerOptions{})
	if _, err := m.Acquire(ctx, "s", "d"); !errors.Is(err, ErrLeaseStoreMissing) {
		t.Fatalf("应返回 ErrLeaseStoreMissing，得到 %v", err)
	}
	if err := m.Release(ctx, "s", "d"); !errors.Is(err, ErrLeaseStoreMissing) {
		t.Fatalf("Release 应返回 ErrLeaseStoreMissing，得到 %v", err)
	}
	if _, err := m.PurgeExpired(ctx); !errors.Is(err, ErrLeaseStoreMissing) {
		t.Fatalf("PurgeExpired 应返回 ErrLeaseStoreMissing，得到 %v", err)
	}
}

// 推送失败不影响租约生效（DB 才是权威）；旧端不在线时静默跳过。
func TestLeaseRevokedPushFailureIsNonFatal(t *testing.T) {
	ctx := context.Background()
	store := newFakeLeaseStore()
	finder := newFakeConnFinder()
	m, _ := newTestLeaseManager(t, store, finder)

	bad := &leaseFakeConn{id: "dev-bad", fail: errors.New("write failed")}
	finder.put(bad)
	if _, err := m.Acquire(ctx, "sess-f", "dev-bad"); err != nil {
		t.Fatal(err)
	}
	// dev-bad 被抢占，推送失败 → Acquire 仍成功
	lease, err := m.Acquire(ctx, "sess-f", "dev-ok")
	if err != nil {
		t.Fatalf("推送失败不应让 Acquire 失败: %v", err)
	}
	if lease.Epoch != 2 || lease.DeviceID != "dev-ok" {
		t.Fatalf("租约应已抢占生效: %+v", lease)
	}
	// 离线旧持有者：无连接 → 不报错
	if _, err := m.Acquire(ctx, "sess-f", "dev-next"); err != nil {
		t.Fatalf("旧持有者离线时不应报错: %v", err)
	}
}

// PurgeExpired 清理过期租约。
func TestLeasePurgeExpired(t *testing.T) {
	ctx := context.Background()
	store := newFakeLeaseStore()
	m, now := newTestLeaseManager(t, store, nil)
	if _, err := m.Acquire(ctx, "sess-p", "dev-p"); err != nil {
		t.Fatal(err)
	}
	if n, err := m.PurgeExpired(ctx); err != nil || n != 0 {
		t.Fatalf("未过期时不应清理: n=%d err=%v", n, err)
	}
	*now = now.Add(5 * time.Minute)
	n, err := m.PurgeExpired(ctx)
	if err != nil || n != 1 {
		t.Fatalf("过期后应清理 1 行: n=%d err=%v", n, err)
	}
}

// 并发 Acquire：epoch 单调递增且不丢更新（DB 由仓储事务保证，这里断言管理器不引入竞态）。
func TestLeaseConcurrentAcquireMonotonic(t *testing.T) {
	ctx := context.Background()
	store := newFakeLeaseStore()
	m, _ := newTestLeaseManager(t, store, nil)

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	epochs := make([]int64, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			lease, err := m.Acquire(ctx, "sess-c", "dev-"+string(rune('a'+i)))
			if err != nil {
				t.Errorf("并发 Acquire 失败: %v", err)
				return
			}
			epochs[i] = lease.Epoch
		}(i)
	}
	wg.Wait()

	final, err := m.Get(ctx, "sess-c")
	if err != nil || final == nil {
		t.Fatalf("最终租约缺失: %+v err=%v", final, err)
	}
	if final.Epoch != int64(n) {
		t.Fatalf("8 个不同持有者依次抢占应 epoch=%d，得到 %d", n, final.Epoch)
	}
	max := int64(0)
	for _, e := range epochs {
		if e > max {
			max = e
		}
	}
	if max != int64(n) {
		t.Fatalf("最大 epoch 应为 %d，得到 %d", n, max)
	}
}

// Active 语义：nil 视为空闲；过期视为无效。
func TestLeaseActive(t *testing.T) {
	if Active(nil, 1) {
		t.Fatal("nil 租约应为非活跃")
	}
	lease := &model.SessionLease{Epoch: 1, ExpiresAt: 100}
	if !Active(lease, 100-1) {
		t.Fatal("未过期应为活跃")
	}
	if Active(lease, 100) {
		t.Fatal("到达 expires_at 应视为过期")
	}
}
