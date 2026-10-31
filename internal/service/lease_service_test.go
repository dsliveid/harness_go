package service

import (
	"context"
	"testing"
	"time"

	"harness-go/internal/model"
	"harness-go/internal/store"
)

// ============ C-6：SessionLease 视图（GET /sessions/{sid}/lease） ============

func newLeaseTestService(t *testing.T) (*DeviceService, *store.MainStore) {
	t.Helper()
	ctx := context.Background()
	main, err := store.OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	svc := NewDeviceService(DeviceServiceOptions{
		Main:    main,
		Enabled: true,
		PairTTL: 120 * time.Second,
		Leases:  NewLeaseAdapter(main, nil),
	})
	return svc, main
}

// 无租约 = active=true / epoch=0 / holder=""（语义：服务端本地可写，不是错误）。
func TestSessionLeaseViewWithoutLease(t *testing.T) {
	svc, _ := newLeaseTestService(t)
	view, err := svc.SessionLease(context.Background(), "sess-x", store.NowMillis())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if view == nil {
		t.Fatal("视图不应为 nil")
	}
	if !view.Active || view.Epoch != 0 || view.Holder != "" {
		t.Fatalf("无租约视图错误: %+v", view)
	}
	if view.HolderKind != string(model.ExecutorServer) {
		t.Fatalf("holder_kind 应为 server，得到 %q", view.HolderKind)
	}
}

// 端侧持有时 holder_kind=device，epoch=1；换设备 epoch+1（读主库真实值）。
func TestSessionLeaseViewDeviceHolder(t *testing.T) {
	svc, _ := newLeaseTestService(t)
	ctx := context.Background()

	if _, err := svc.Leases().Acquire(ctx, "sess-y", "dev-1"); err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	view, err := svc.SessionLease(ctx, "sess-y", store.NowMillis())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if view.Holder != "dev-1" || view.Epoch != 1 || !view.Active {
		t.Fatalf("设备持有视图错误: %+v", view)
	}
	if view.HolderKind != string(model.ExecutorDevice) {
		t.Fatalf("holder_kind 应为 device，得到 %q", view.HolderKind)
	}
	if view.ExpiresAt == 0 || view.HeartbeatAt == 0 {
		t.Fatalf("持有中应带时间戳: %+v", view)
	}

	// 第二台设备抢占 → epoch=2
	if _, err := svc.Leases().Acquire(ctx, "sess-y", "dev-2"); err != nil {
		t.Fatalf("抢占失败: %v", err)
	}
	view2, err := svc.SessionLease(ctx, "sess-y", store.NowMillis())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if view2.Holder != "dev-2" || view2.Epoch != 2 {
		t.Fatalf("抢占后视图错误: %+v", view2)
	}
}

// 服务端持有时 holder 为空串但 holder_kind=server（引擎 Run 期间的真实状态）。
func TestSessionLeaseViewServerHolder(t *testing.T) {
	svc, _ := newLeaseTestService(t)
	ctx := context.Background()

	if _, err := svc.Leases().Acquire(ctx, "sess-z", ""); err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	view, err := svc.SessionLease(ctx, "sess-z", store.NowMillis())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if view.Holder != "" || view.HolderKind != string(model.ExecutorServer) {
		t.Fatalf("服务端持有视图错误: %+v", view)
	}
	if view.Epoch != 1 || !view.Active {
		t.Fatalf("服务端持有应 epoch=1 且 active: %+v", view)
	}
}

// 过期租约：active=false（不是错误，也不是删除）。
func TestSessionLeaseViewExpired(t *testing.T) {
	svc, _ := newLeaseTestService(t)
	ctx := context.Background()

	if _, err := svc.Leases().Acquire(ctx, "sess-e", "dev-e"); err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}
	// 用「远未来」的 now 查询 → 判定为过期
	future := store.NowMillis() + int64(2*time.Hour/time.Millisecond)
	view, err := svc.SessionLease(ctx, "sess-e", future)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if view.Active {
		t.Fatalf("超期查询应返回 active=false: %+v", view)
	}
	if view.Holder != "dev-e" || view.Epoch != 1 {
		t.Fatalf("过期视图应保留原归属信息: %+v", view)
	}
}

// 未装配租约（设备域关闭）→ 视为服务端独占，不报错。
func TestSessionLeaseViewWithoutLeaseManager(t *testing.T) {
	svc := NewDeviceService(DeviceServiceOptions{})
	view, err := svc.SessionLease(context.Background(), "sess-n", store.NowMillis())
	if err != nil {
		t.Fatalf("未装配租约不应报错: %v", err)
	}
	if !view.Active || view.HolderKind != string(model.ExecutorServer) {
		t.Fatalf("未装配应视为服务端可写: %+v", view)
	}
}

// 空会话 ID 报参数错误。
func TestSessionLeaseViewRejectsEmptySession(t *testing.T) {
	svc, _ := newLeaseTestService(t)
	if _, err := svc.SessionLease(context.Background(), "  ", store.NowMillis()); err == nil {
		t.Fatal("空会话 ID 应报参数错误")
	}
}
