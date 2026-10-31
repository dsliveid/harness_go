package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"harness-go/internal/model"
)

// 主库 0002 迁移：4 张设备表齐备、关键列存在、版本线推进到 2。
func TestMainDevicesMigration(t *testing.T) {
	ctx := context.Background()
	m, err := OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("OpenMain 失败: %v", err)
	}
	defer m.Close()

	for _, tbl := range []string{"devices", "device_pair_codes", "capabilities", "session_leases"} {
		var cnt int
		if err := m.conn().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&cnt); err != nil {
			t.Fatal(err)
		}
		if cnt != 1 {
			t.Fatalf("主库缺少设备表 %s", tbl)
		}
	}

	// 多端协同关键列必须存在（devices）
	for _, col := range []string{"kind", "platform", "status", "capabilities_hash",
		"protocol_version", "app_version", "load_json", "token_hash", "paired_at",
		"last_seen_at", "revoked_at"} {
		var cnt int
		if err := m.conn().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info('devices') WHERE name=?`, col).Scan(&cnt); err != nil {
			t.Fatal(err)
		}
		if cnt != 1 {
			t.Fatalf("devices 缺少列 %s", col)
		}
	}

	// 配对码一次性消费依赖 consumed_at + expires_at
	for _, col := range []string{"expires_at", "consumed_at", "device_name", "device_kind"} {
		var cnt int
		if err := m.conn().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info('device_pair_codes') WHERE name=?`, col).Scan(&cnt); err != nil {
			t.Fatal(err)
		}
		if cnt != 1 {
			t.Fatalf("device_pair_codes 缺少列 %s", col)
		}
	}

	// 租约 epoch / expires_at 是抢占语义的核心
	for _, col := range []string{"device_id", "epoch", "expires_at", "heartbeat_at"} {
		var cnt int
		if err := m.conn().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info('session_leases') WHERE name=?`, col).Scan(&cnt); err != nil {
			t.Fatal(err)
		}
		if cnt != 1 {
			t.Fatalf("session_leases 缺少列 %s", col)
		}
	}

	v, err := SchemaVersion(ctx, m.conn())
	if err != nil {
		t.Fatal(err)
	}
	// 0004_market.sql 落地后主库版本为 4。
	if v != 5 {
		t.Fatalf("主库 schema 版本期望 5，得到 %d", v)
	}
	req, err := RequiredSchemaVersion("main")
	if err != nil {
		t.Fatal(err)
	}
	if req != 5 {
		t.Fatalf("RequiredSchemaVersion(main) 期望 5，得到 %d", req)
	}
}

// 会话库 0002 迁移：approvals 增跨端审计列，表数量不变（9）。
func TestSessionApprovalAuditMigration(t *testing.T) {
	ctx := context.Background()
	ss, err := OpenSessionStore(SessionStoreOptions{DataDir: t.TempDir(), MaxOpen: 4})
	if err != nil {
		t.Fatalf("OpenSessionStore 失败: %v", err)
	}
	defer ss.Close()

	sdb, err := ss.Acquire(ctx, "sess-c1-audit")
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}

	for _, col := range []string{"requested_by_device", "decided_by_device"} {
		var cnt int
		if err := sdb.conn().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info('approvals') WHERE name=?`, col).Scan(&cnt); err != nil {
			t.Fatal(err)
		}
		if cnt != 1 {
			t.Fatalf("approvals 缺少审计列 %s", col)
		}
	}

	// 仅加列不加表
	var n int
	if err := sdb.conn().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 12 {
		t.Fatalf("会话库表数量期望 12，得到 %d", n)
	}

	v, err := SchemaVersion(ctx, sdb.conn())
	if err != nil {
		t.Fatal(err)
	}
	if v != 4 {
		t.Fatalf("会话库 schema 版本期望 4，得到 %d", v)
	}
}

func newDeviceFixture() *model.Device {
	return &model.Device{
		ID:               "dev-1",
		Name:             "我的笔记本",
		Kind:             model.DeviceKindDesktop,
		Platform:         "windows",
		AppVersion:       "0.9.0",
		TokenHash:        "hash-abc",
		LoadJSON:         json.RawMessage(`{"cpu":0.2}`),
		CapabilitiesHash: "cap-hash-1",
	}
}

// 设备仓储全链路：创建 → 查询 → 在线/心跳 → 离线 → 吊销。
func TestDeviceRepoLifecycle(t *testing.T) {
	ctx := context.Background()
	m, err := OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("OpenMain 失败: %v", err)
	}
	defer m.Close()
	repo := m.Devices()

	d := newDeviceFixture()
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}
	if d.ProtocolVersion != model.DefaultDeviceProtocolVersion {
		t.Fatalf("协议版本应回填默认值 %d，得到 %d", model.DefaultDeviceProtocolVersion, d.ProtocolVersion)
	}
	if d.Status != model.DeviceOffline {
		t.Fatalf("新配对设备初始状态应为 offline，得到 %s", d.Status)
	}

	got, err := repo.Get(ctx, "dev-1")
	if err != nil {
		t.Fatalf("读取设备失败: %v", err)
	}
	if got.Name != "我的笔记本" || got.Kind != model.DeviceKindDesktop || got.Platform != "windows" {
		t.Fatalf("设备字段错误: %+v", got)
	}
	if string(got.LoadJSON) != `{"cpu":0.2}` {
		t.Fatalf("load_json 未还原: %s", got.LoadJSON)
	}

	// 按 token_hash 反查（WS 升级前鉴权路径）
	byTok, err := repo.GetByTokenHash(ctx, "hash-abc")
	if err != nil {
		t.Fatalf("按令牌摘要查设备失败: %v", err)
	}
	if byTok.ID != "dev-1" {
		t.Fatalf("令牌反查命中错误设备: %s", byTok.ID)
	}
	if _, err := repo.GetByTokenHash(ctx, "hash-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知令牌应返回 ErrNotFound，得到 %v", err)
	}

	if _, err := repo.Get(ctx, "dev-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的设备应返回 ErrNotFound，得到 %v", err)
	}

	now := NowMillis()
	if err := repo.MarkOnline(ctx, "dev-1", now); err != nil {
		t.Fatalf("标记在线失败: %v", err)
	}
	got, _ = repo.Get(ctx, "dev-1")
	if !got.Online() || got.LastSeenAt == nil || *got.LastSeenAt != now {
		t.Fatalf("在线状态/心跳未写入: %+v", got)
	}

	// 心跳携带新的能力摘要与负载
	if err := repo.UpdateHeartbeat(ctx, "dev-1", now+15000,
		json.RawMessage(`{"cpu":0.5}`), "cap-hash-2", 1); err != nil {
		t.Fatalf("刷新心跳失败: %v", err)
	}
	got, _ = repo.Get(ctx, "dev-1")
	if got.CapabilitiesHash != "cap-hash-2" || string(got.LoadJSON) != `{"cpu":0.5}` {
		t.Fatalf("心跳未更新能力摘要/负载: %+v", got)
	}
	if got.LastSeenAt == nil || *got.LastSeenAt != now+15000 {
		t.Fatalf("心跳时间未刷新: %+v", got.LastSeenAt)
	}
	// 空负载与空摘要不应覆盖既有值
	if err := repo.UpdateHeartbeat(ctx, "dev-1", now+30000, nil, "", 0); err != nil {
		t.Fatalf("刷空心跳失败: %v", err)
	}
	got, _ = repo.Get(ctx, "dev-1")
	if got.CapabilitiesHash != "cap-hash-2" || string(got.LoadJSON) != `{"cpu":0.5}` {
		t.Fatalf("空心跳不应覆盖既有值: %+v", got)
	}

	if err := repo.MarkOffline(ctx, "dev-1", now+45000); err != nil {
		t.Fatalf("标记离线失败: %v", err)
	}
	got, _ = repo.Get(ctx, "dev-1")
	if got.Status != model.DeviceOffline {
		t.Fatalf("状态应为 offline，得到 %s", got.Status)
	}

	// 列表与计数
	if err := repo.Create(ctx, &model.Device{ID: "dev-2", Name: "手机",
		Kind: model.DeviceKindMobile, Platform: "android", TokenHash: "hash-def"}); err != nil {
		t.Fatalf("创建第二台设备失败: %v", err)
	}
	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("列出设备失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("设备数期望 2，得到 %d", len(list))
	}
	if n, err := repo.Count(ctx); err != nil || n != 2 {
		t.Fatalf("设备计数期望 2，得到 %d (err=%v)", n, err)
	}

	// 吊销：终态 + 令牌即刻失效
	if err := repo.Revoke(ctx, "dev-1", now+60000); err != nil {
		t.Fatalf("吊销设备失败: %v", err)
	}
	got, _ = repo.Get(ctx, "dev-1")
	if got.Status != model.DeviceRevoked || got.RevokedAt == nil {
		t.Fatalf("吊销状态未写入: %+v", got)
	}
	if _, err := repo.GetByTokenHash(ctx, "hash-abc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("吊销后令牌应失效（ErrNotFound），得到 %v", err)
	}
	if err := repo.Revoke(ctx, "dev-1", now+61000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复吊销应返回 ErrNotFound，得到 %v", err)
	}
	if err := repo.MarkOnline(ctx, "dev-1", now+62000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("已吊销设备不得回到在线，得到 %v", err)
	}

	// 物理删除（连带能力项）
	if err := m.Capabilities().Replace(ctx, "dev-1", []model.Capability{{Name: "fs.read"}}); err != nil {
		t.Fatalf("写入能力失败: %v", err)
	}
	if err := repo.Delete(ctx, "dev-1"); err != nil {
		t.Fatalf("删除设备失败: %v", err)
	}
	if _, err := repo.Get(ctx, "dev-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后应 ErrNotFound，得到 %v", err)
	}
	caps, err := m.Capabilities().ListByDevice(ctx, "dev-1")
	if err != nil {
		t.Fatalf("查询能力失败: %v", err)
	}
	if len(caps) != 0 {
		t.Fatalf("删除设备应连带删除能力项，残留 %d 条", len(caps))
	}
}

// 配对码原子消费：并发下只允许一个成功（一次性语义）。
func TestPairCodeAtomicConsume(t *testing.T) {
	ctx := context.Background()
	m, err := OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("OpenMain 失败: %v", err)
	}
	defer m.Close()
	repo := m.PairCodes()

	now := NowMillis()
	pc := &model.DevicePairCode{
		Code:       "482913",
		DeviceName: "",
		DeviceKind: model.DeviceKindDesktop,
		ExpiresAt:  now + 120000, // TTL 120s
	}
	if err := repo.Create(ctx, pc); err != nil {
		t.Fatalf("生成配对码失败: %v", err)
	}

	// 先并发抢消费：恰好一次成功
	const workers = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := repo.Consume(ctx, "482913", NowMillis())
			if err != nil {
				return // 锁竞争产生的错误不计入成功
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("配对码并发消费成功次数期望 1，得到 %d", wins)
	}

	// 已消费：再次消费必须失败（且不返回普通错误，便于上层统一 401 防枚举）
	ok, err := repo.Consume(ctx, "482913", NowMillis())
	if err != nil {
		t.Fatalf("重复消费不应返回基础设施错误: %v", err)
	}
	if ok {
		t.Fatal("配对码已消费却再次成功")
	}

	// consumed_at 已落库
	got, err := repo.Get(ctx, "482913")
	if err != nil {
		t.Fatalf("读取配对码失败: %v", err)
	}
	if got.ConsumedAt == nil {
		t.Fatal("consumed_at 未落库")
	}
	if got.Usable(NowMillis()) {
		t.Fatal("已消费的码不应 Usable")
	}
	if got.DeviceKind != model.DeviceKindDesktop {
		t.Fatalf("device_kind 未还原: %s", got.DeviceKind)
	}

	// 过期码：消费失败且不报错
	if err := repo.Create(ctx, &model.DevicePairCode{
		Code: "111111", ExpiresAt: now - 1000,
	}); err != nil {
		t.Fatalf("生成过期码失败: %v", err)
	}
	ok, err = repo.Consume(ctx, "111111", now)
	if err != nil {
		t.Fatalf("过期码消费不应返回基础设施错误: %v", err)
	}
	if ok {
		t.Fatal("过期码消费成功（不应发生）")
	}

	// 不存在的码：同样返回 (false, nil)，与已消费/过期不可区分（防枚举）
	ok, err = repo.Consume(ctx, "999999", now)
	if err != nil {
		t.Fatalf("不存在码消费不应返回基础设施错误: %v", err)
	}
	if ok {
		t.Fatal("不存在的码消费成功（不应发生）")
	}
	if _, err := repo.Get(ctx, "999999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在码 Get 应返回 ErrNotFound，得到 %v", err)
	}

	// 过期清理
	n, err := repo.PurgeExpired(ctx, now)
	if err != nil {
		t.Fatalf("清理过期码失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("清理过期码期望 1 条，得到 %d", n)
	}
}

// 能力项按设备全量替换（先删后插，不残留旧条目）。
func TestCapabilityReplace(t *testing.T) {
	ctx := context.Background()
	m, err := OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("OpenMain 失败: %v", err)
	}
	defer m.Close()
	repo := m.Capabilities()

	if err := repo.Replace(ctx, "dev-1", []model.Capability{
		{Name: "fs.read", Version: "1"},
		{Name: "fs.write", Version: "1"},
		{Name: "exec", Version: "1"},
	}); err != nil {
		t.Fatalf("写入能力失败: %v", err)
	}
	caps, err := repo.ListByDevice(ctx, "dev-1")
	if err != nil {
		t.Fatalf("查询能力失败: %v", err)
	}
	if len(caps) != 3 {
		t.Fatalf("能力数期望 3，得到 %d", len(caps))
	}
	if caps[0].Name != "exec" { // 按名称升序
		t.Fatalf("能力未按名称排序: %+v", caps)
	}

	// 第二次替换：只留 1 条，旧条目必须消失
	if err := repo.Replace(ctx, "dev-1", []model.Capability{{Name: "browser", Version: "2"}}); err != nil {
		t.Fatalf("第二次替换失败: %v", err)
	}
	caps, _ = repo.ListByDevice(ctx, "dev-1")
	if len(caps) != 1 || caps[0].Name != "browser" || caps[0].Version != "2" {
		t.Fatalf("能力全量替换未生效: %+v", caps)
	}

	// 空集合 = 清空
	if err := repo.Replace(ctx, "dev-1", nil); err != nil {
		t.Fatalf("清空能力失败: %v", err)
	}
	caps, _ = repo.ListByDevice(ctx, "dev-1")
	if len(caps) != 0 {
		t.Fatalf("清空后仍残留 %d 条", len(caps))
	}

	// 缺少 device_id 时报错
	if err := repo.Replace(ctx, "", []model.Capability{{Name: "x"}}); err == nil {
		t.Fatal("缺少 device_id 应报错")
	}
}

// 租约：首次建立 epoch=1；同持有者续期不涨；换人抢占 epoch+1；过期接管亦 +1。
func TestLeaseAcquireEpoch(t *testing.T) {
	ctx := context.Background()
	m, err := OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("OpenMain 失败: %v", err)
	}
	defer m.Close()
	repo := m.Leases()

	now := NowMillis()
	const ttl = int64(60000)

	// 首次：device-A 建立租约
	l1, changed, err := repo.Acquire(ctx, "s1", "dev-A", now, ttl)
	if err != nil {
		t.Fatalf("首次 Acquire 失败: %v", err)
	}
	if l1.Epoch != 1 {
		t.Fatalf("首次租约 epoch 期望 1，得到 %d", l1.Epoch)
	}
	if !changed {
		t.Fatal("首次由设备接管应标记 changed")
	}
	if !l1.HeldBy("dev-A", now) {
		t.Fatalf("dev-A 应持有租约: %+v", l1)
	}

	// 同持有者续期：epoch 不变、不产生 revoked 通知
	l2, changed, err := repo.Acquire(ctx, "s1", "dev-A", now+1000, ttl)
	if err != nil {
		t.Fatalf("续期失败: %v", err)
	}
	if l2.Epoch != 1 {
		t.Fatalf("同一持有者续期 epoch 应保持 1，得到 %d", l2.Epoch)
	}
	if changed {
		t.Fatal("同一持有者续期不应标记 changed")
	}
	if l2.ExpiresAt <= l1.ExpiresAt {
		t.Fatalf("续期应延长过期时间: %d -> %d", l1.ExpiresAt, l2.ExpiresAt)
	}

	// 换人抢占：epoch+1，且必须标记 changed（用于推 lease.revoked）
	l3, changed, err := repo.Acquire(ctx, "s1", "dev-B", now+2000, ttl)
	if err != nil {
		t.Fatalf("抢占失败: %v", err)
	}
	if l3.Epoch != 2 {
		t.Fatalf("抢占后 epoch 期望 2，得到 %d", l3.Epoch)
	}
	if !changed {
		t.Fatal("跨设备抢占必须标记 changed")
	}
	got, err := repo.Get(ctx, "s1")
	if err != nil {
		t.Fatalf("查询租约失败: %v", err)
	}
	if got.DeviceID != "dev-B" || got.Epoch != 2 {
		t.Fatalf("库内租约未更新: %+v", got)
	}
	if got.HeldBy("dev-A", now+2000) {
		t.Fatal("旧持有者不应仍持有租约")
	}

	// 过期后同设备接管：也应 epoch+1（旧租约失效，需重新申领）
	expiredAt := got.ExpiresAt
	l4, _, err := repo.Acquire(ctx, "s1", "dev-B", expiredAt+1, ttl)
	if err != nil {
		t.Fatalf("过期接管失败: %v", err)
	}
	if l4.Epoch != 3 {
		t.Fatalf("过期接管 epoch 期望 3，得到 %d", l4.Epoch)
	}

	// 非持有者释放无效
	released, err := repo.Release(ctx, "s1", "dev-A")
	if err != nil {
		t.Fatalf("释放租约失败: %v", err)
	}
	if released {
		t.Fatal("非持有者不应释放成功")
	}
	released, err = repo.Release(ctx, "s1", "dev-B")
	if err != nil {
		t.Fatalf("持有者释放失败: %v", err)
	}
	if !released {
		t.Fatal("持有者释放应成功")
	}
	if _, err := repo.Get(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("释放后应 ErrNotFound，得到 %v", err)
	}

	// 过期租约批量清理
	if _, _, err := repo.Acquire(ctx, "s2", "dev-A", now, ttl); err != nil {
		t.Fatalf("s2 Acquire 失败: %v", err)
	}
	n, err := repo.DeleteExpired(ctx, now+2*ttl)
	if err != nil {
		t.Fatalf("清理过期租约失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("清理过期租约期望 1 条，得到 %d", n)
	}

	// 参数校验
	if _, _, err := repo.Acquire(ctx, "", "dev-A", now, ttl); err == nil {
		t.Fatal("缺少 session_id 应报错")
	}
	if _, _, err := repo.Acquire(ctx, "s3", "dev-A", now, 0); err == nil {
		t.Fatal("ttl <= 0 应报错")
	}
}

// 既有 v1 库增量升级到 v2：两套版本线都只追加不重建，旧数据必须保留。
func TestMigrationUpgradeFromV1(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// ---------- 主库：手工停在 v1 ----------
	mainPath := dir + "/main.db"
	mdb, err := openDB(mainPath, 4)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	// defer 兜底：断言中途 Fatalf 时也要释放句柄，否则 Windows 上 t.TempDir 清理会报
	// "The process cannot access the file ... main.db"（句柄仍被占用）。
	defer func() { _ = mdb.Close() }()
	if err := ensureMigrationTable(ctx, mdb); err != nil {
		t.Fatal(err)
	}
	v1, err := loadMigrations("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(v1) < 1 || v1[0].version != 1 {
		t.Fatalf("主库首个迁移应为 version 1: %+v", v1)
	}
	if _, err := mdb.ExecContext(ctx, v1[0].sql); err != nil {
		t.Fatalf("应用主库 0001 失败: %v", err)
	}
	if _, err := mdb.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, name, applied_at) VALUES(1,?,?)`,
		v1[0].name, NowMillis()); err != nil {
		t.Fatal(err)
	}
	// v1 时代写入一条会话数据
	if _, err := mdb.ExecContext(ctx,
		`INSERT INTO sessions(id, title, created_at, updated_at) VALUES('old-s1','老会话',1,1)`); err != nil {
		t.Fatal(err)
	}

	applied, err := applyMigrations(ctx, mdb, "main")
	if err != nil {
		t.Fatalf("主库增量迁移失败: %v", err)
	}
	// v1 库升级：0002_devices + 0003_retrieval + 0004_market + 0005_snapshots 共 4 个增量迁移。
	if applied != 4 {
		t.Fatalf("主库增量迁移应应用 4 个，得到 %d", applied)
	}
	if v, _ := SchemaVersion(ctx, mdb); v != 5 {
		t.Fatalf("主库升级后版本期望 5，得到 %d", v)
	}
	var title string
	if err := mdb.QueryRowContext(ctx, `SELECT title FROM sessions WHERE id='old-s1'`).Scan(&title); err != nil {
		t.Fatalf("v1 旧数据丢失: %v", err)
	}
	if title != "老会话" {
		t.Fatalf("v1 旧数据被篡改: %s", title)
	}
	// 再跑一次必须幂等
	if applied, err := applyMigrations(ctx, mdb, "main"); err != nil || applied != 0 {
		t.Fatalf("主库迁移不幂等: applied=%d err=%v", applied, err)
	}
	_ = mdb.Close()

	// ---------- 会话库：手工停在 v1，且已有审批记录 ----------
	rawPath := filepath.Join(dir, "sessions", "sess-upgrade", "session.db")
	sdb, err := openDB(rawPath, 1)
	if err != nil {
		t.Fatalf("打开会话库失败: %v", err)
	}
	// defer 兜底：同上，避免断言失败时句柄泄漏导致 TempDir 清理报占用。
	defer func() { _ = sdb.Close() }()
	if err := ensureMigrationTable(ctx, sdb); err != nil {
		t.Fatal(err)
	}
	sv1, err := loadMigrations("session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sdb.ExecContext(ctx, sv1[0].sql); err != nil {
		t.Fatalf("应用会话库 0001 失败: %v", err)
	}
	if _, err := sdb.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, name, applied_at) VALUES(1,?,?)`,
		sv1[0].name, NowMillis()); err != nil {
		t.Fatal(err)
	}
	if _, err := sdb.ExecContext(ctx,
		`INSERT INTO approvals(id, run_id, tool_event_id, request_json, decision, requested_at)
		 VALUES('ap-1','r-1','te-1','{}','allow_once',1)`); err != nil {
		t.Fatal(err)
	}
	if applied, err := applyMigrations(ctx, sdb, "session"); err != nil || applied != 3 {
		t.Fatalf("会话库增量迁移失败: applied=%d err=%v", applied, err)
	}
	// 旧审批行保留，新列默认 NULL
	var (
		decision  sql.NullString
		reqDevice sql.NullString
		decDevice sql.NullString
	)
	if err := sdb.QueryRowContext(ctx,
		`SELECT decision, requested_by_device, decided_by_device FROM approvals WHERE id='ap-1'`).
		Scan(&decision, &reqDevice, &decDevice); err != nil {
		t.Fatalf("v1 审批记录丢失: %v", err)
	}
	if decision.String != "allow_once" || reqDevice.Valid || decDevice.Valid {
		t.Fatalf("审计列应为 NULL 且旧值保留: decision=%v req=%v dec=%v",
			decision, reqDevice, decDevice)
	}
	_ = sdb.Close()
}
