package device

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"harness-go/internal/model"
)

// fakePairCodeStore 内存实现 PairCodeStore，语义与主库仓储对齐：
// Consume 必须"存在 + 未消费 + 未过期"三条同时满足才返回 true。
type fakePairCodeStore struct {
	mu   sync.Mutex
	rows map[string]*model.DevicePairCode
}

func newFakePairCodeStore() *fakePairCodeStore {
	return &fakePairCodeStore{rows: make(map[string]*model.DevicePairCode)}
}

var errFakeNotFound = errors.New("fake: 记录不存在")

func (f *fakePairCodeStore) Create(_ context.Context, pc *model.DevicePairCode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.rows[pc.Code]; dup {
		return errors.New("fake: 主键冲突")
	}
	clone := *pc
	f.rows[pc.Code] = &clone
	return nil
}

func (f *fakePairCodeStore) Get(_ context.Context, code string) (*model.DevicePairCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pc, ok := f.rows[code]
	if !ok {
		return nil, errFakeNotFound
	}
	clone := *pc
	return &clone, nil
}

func (f *fakePairCodeStore) Consume(_ context.Context, code string, now int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pc, ok := f.rows[code]
	if !ok || pc.ConsumedAt != nil || pc.ExpiresAt <= now {
		return false, nil
	}
	at := now
	pc.ConsumedAt = &at
	return true, nil
}

func (f *fakePairCodeStore) PurgeExpired(_ context.Context, now int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for code, pc := range f.rows {
		if pc.ExpiresAt <= now {
			delete(f.rows, code)
			n++
		}
	}
	return n, nil
}

// fixedClock 返回可控时钟。
func fixedClock(start time.Time) func() time.Time {
	var mu sync.Mutex
	cur := start
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}
}

// TestPairCodeTTL120s 断言配对码有效期恰为 120s（铁律，不可漂移）。
func TestPairCodeTTL120s(t *testing.T) {
	store := newFakePairCodeStore()
	issuer := NewPairCodeIssuer(store, 0) // 传 0 → 回退默认
	if issuer.TTL() != 120*time.Second {
		t.Fatalf("配对码默认 TTL 必须为 120s，得到 %s", issuer.TTL())
	}

	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	issuer.SetClock(fixedClock(base))

	pc, err := issuer.Issue(context.Background(), "我的桌面端", model.DeviceKindDesktop)
	if err != nil {
		t.Fatalf("生成配对码失败: %v", err)
	}
	if !ValidPairCode(pc.Code) {
		t.Fatalf("配对码应为 6 位数字，得到 %q", pc.Code)
	}
	wantExpires := base.Add(120 * time.Second).UnixMilli()
	if pc.ExpiresAt != wantExpires {
		t.Fatalf("expires_at 应为 签发时刻+120s（%d），得到 %d", wantExpires, pc.ExpiresAt)
	}
	if pc.DeviceName != "我的桌面端" || pc.DeviceKind != model.DeviceKindDesktop {
		t.Fatalf("预置提示未保留: %+v", pc)
	}
}

// TestPairCodeExpiredRejected 过期后兑换失败，且与"不存在"同错误（防枚举）。
func TestPairCodeExpiredRejected(t *testing.T) {
	store := newFakePairCodeStore()
	issuer := NewPairCodeIssuer(store, 120*time.Second)

	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	clock := &movableClock{cur: now}
	issuer.SetClock(clock.Now)

	pc, err := issuer.Issue(context.Background(), "", model.DeviceKindMobile)
	if err != nil {
		t.Fatalf("生成配对码失败: %v", err)
	}
	// 119s 仍可兑换
	clock.Advance(119 * time.Second)
	if _, err := issuer.Redeem(context.Background(), pc.Code); err != nil {
		t.Fatalf("119s 时应仍在有效期内: %v", err)
	}

	// 新码跨过 120s → 拒绝
	pc2, err := issuer.Issue(context.Background(), "", model.DeviceKindMobile)
	if err != nil {
		t.Fatalf("生成配对码失败: %v", err)
	}
	clock.Advance(121 * time.Second)
	if _, err := issuer.Redeem(context.Background(), pc2.Code); !errors.Is(err, ErrPairCodeInvalid) {
		t.Fatalf("过期码应返回 ErrPairCodeInvalid，得到 %v", err)
	}
}

// TestPairCodeSingleUse 一次性消费：同一码第二次兑换必须失败。
func TestPairCodeSingleUse(t *testing.T) {
	store := newFakePairCodeStore()
	issuer := NewPairCodeIssuer(store, 120*time.Second)

	pc, err := issuer.Issue(context.Background(), "", model.DeviceKindCLI)
	if err != nil {
		t.Fatalf("生成配对码失败: %v", err)
	}
	if _, err := issuer.Redeem(context.Background(), pc.Code); err != nil {
		t.Fatalf("首次兑换应成功: %v", err)
	}
	if _, err := issuer.Redeem(context.Background(), pc.Code); !errors.Is(err, ErrPairCodeInvalid) {
		t.Fatalf("二次兑换应返回 ErrPairCodeInvalid，得到 %v", err)
	}
}

// TestPairCodeConcurrentRedeemOnlyOnce 并发兑换同一码只允许 1 次成功（原子性）。
func TestPairCodeConcurrentRedeemOnlyOnce(t *testing.T) {
	store := newFakePairCodeStore()
	issuer := NewPairCodeIssuer(store, 120*time.Second)

	pc, err := issuer.Issue(context.Background(), "", model.DeviceKindDesktop)
	if err != nil {
		t.Fatalf("生成配对码失败: %v", err)
	}

	const n = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		success int
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := issuer.Redeem(context.Background(), pc.Code); err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("并发兑换同一码应仅 1 次成功，实际 %d 次", success)
	}
}

// TestPairCodeAntiEnumeration 不存在 / 已消费 / 已过期 / 格式非法 → 同一错误，不可区分。
func TestPairCodeAntiEnumeration(t *testing.T) {
	store := newFakePairCodeStore()
	issuer := NewPairCodeIssuer(store, 120*time.Second)
	clock := &movableClock{cur: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)}
	issuer.SetClock(clock.Now)

	// 已消费
	used, err := issuer.Issue(context.Background(), "", model.DeviceKindDesktop)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Redeem(context.Background(), used.Code); err != nil {
		t.Fatal(err)
	}
	// 已过期
	expired, err := issuer.Issue(context.Background(), "", model.DeviceKindDesktop)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(121 * time.Second)

	cases := []string{
		"000000",     // 大概率不存在（若恰好撞上已消费码也无妨，仍是同一错误）
		used.Code,    // 已消费
		expired.Code, // 已过期
		"12",         // 格式非法
		"abcdef",     // 格式非法
		"",           // 空
	}
	for _, code := range cases {
		if _, err := issuer.Redeem(context.Background(), code); !errors.Is(err, ErrPairCodeInvalid) {
			t.Fatalf("码 %q 应返回 ErrPairCodeInvalid，得到 %v", code, err)
		}
	}
}

// TestValidPairCode 形状校验。
func TestValidPairCode(t *testing.T) {
	cases := map[string]bool{
		"482913":  true,
		"000000":  true,
		"48291":   false,
		"4829130": false,
		"48a913":  false,
		" 48291":  false,
		"":        false,
	}
	for code, want := range cases {
		if got := ValidPairCode(code); got != want {
			t.Fatalf("ValidPairCode(%q) = %v，期望 %v", code, got, want)
		}
	}
}

// TestDeviceTokenHashAndCapabilitiesHash 令牌摘要与能力摘要稳定性。
func TestDeviceTokenHashAndCapabilitiesHash(t *testing.T) {
	token, err := NewDeviceToken()
	if err != nil {
		t.Fatalf("生成设备令牌失败: %v", err)
	}
	if len(token) <= len(DeviceTokenPrefix) || token[:len(DeviceTokenPrefix)] != DeviceTokenPrefix {
		t.Fatalf("设备令牌应以 %q 开头，得到 %q", DeviceTokenPrefix, token)
	}
	sum := HashDeviceToken(token)
	if len(sum) != 64 {
		t.Fatalf("sha256 hex 应为 64 字符，得到 %d", len(sum))
	}
	if sum == token || HashDeviceToken(token) != sum {
		t.Fatal("令牌摘要必须稳定且不等于明文")
	}

	a := CapabilitiesHash([]model.Capability{{Name: "fs.read", Version: "1"}, {Name: "exec", Version: "2"}})
	b := CapabilitiesHash([]model.Capability{{Name: "exec", Version: "2"}, {Name: "fs.read", Version: "1"}})
	if a != b {
		t.Fatal("能力摘要应与顺序无关")
	}
	c := CapabilitiesHash([]model.Capability{{Name: "exec", Version: "2"}})
	if a == c {
		t.Fatal("能力集合不同时摘要必须不同")
	}
}

// movableClock 可推进的时钟（测试用）。
type movableClock struct {
	mu  sync.Mutex
	cur time.Time
}

func (c *movableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

func (c *movableClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur = c.cur.Add(d)
}
