package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// newDeviceTestService 建主库 + 设备用例（TTL 120s，与线上默认一致）。
func newDeviceTestService(t *testing.T) (*DeviceService, *store.MainStore) {
	t.Helper()
	dir := t.TempDir()
	main, err := store.OpenMain(context.Background(), dir)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	svc := NewDeviceService(DeviceServiceOptions{
		Main:    main,
		Enabled: true,
		PairTTL: 120 * time.Second,
	})
	// 固定时钟到秒级，便于断言 expires_at
	svc.SetPairClock(func() time.Time {
		return time.UnixMilli(time.Now().UnixMilli())
	})
	return svc, main
}

// TestDevicePairHappyPath 配对 → 列表 → 吊销 全链路。
func TestDevicePairHappyPath(t *testing.T) {
	svc, _ := newDeviceTestService(t)
	ctx := context.Background()

	view, err := svc.IssuePairCode(ctx, "办公机", model.DeviceKindDesktop)
	if err != nil {
		t.Fatalf("生成配对码失败: %v", err)
	}
	if len(view.Code) != 6 || view.TTLSec != 120 {
		t.Fatalf("配对码应为 6 位且 TTL 120s，得到 %+v", view)
	}

	res, err := svc.Pair(ctx, "10.0.0.1", PairRequest{
		Code:     view.Code,
		Name:     "我的 MacBook",
		Kind:     model.DeviceKindDesktop,
		Platform: "darwin",
		Capabilities: []model.Capability{
			{Name: "fs.read", Version: "1.0"},
			{Name: "exec", Version: "2.0"},
			{Name: "fs.read", Version: "9.9"}, // 同名去重，保留首个
			{Name: "  ", Version: "x"},        // 空名丢弃
		},
	})
	if err != nil {
		t.Fatalf("配对失败: %v", err)
	}
	if res.DeviceID == "" || res.DeviceToken == "" {
		t.Fatalf("配对结果缺少 device_id / device_token: %+v", res)
	}
	if res.WSPath != DeviceWSPath || res.ProtocolVersion != model.DefaultDeviceProtocolVersion {
		t.Fatalf("配对结果协议字段错误: %+v", res)
	}

	// 列表：令牌摘要绝不出栈，能力项已落库
	list, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("设备列表失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("应有 1 台设备，得到 %d", len(list))
	}
	d := list[0]
	if d.TokenHash != "" {
		t.Fatal("设备列表绝不允许回传令牌摘要")
	}
	if d.Name != "我的 MacBook" || d.Platform != "darwin" || !d.Kind.Valid() {
		t.Fatalf("设备基础字段错误: %+v", d)
	}
	if d.Status != model.DeviceOffline {
		t.Fatalf("配对完成应为 offline（等 WS 上线），得到 %s", d.Status)
	}
	if len(d.Capabilities) != 2 {
		t.Fatalf("能力项应为 2 条（去重 + 丢弃空名），得到 %d: %+v", len(d.Capabilities), d.Capabilities)
	}
	if d.CapabilitiesHash == "" {
		t.Fatal("能力摘要不应为空")
	}

	// 令牌鉴权可用于 WS 升级前校验
	authed, err := svc.Authenticate(ctx, res.DeviceToken)
	if err != nil {
		t.Fatalf("设备令牌鉴权应成功: %v", err)
	}
	if authed.ID != res.DeviceID {
		t.Fatalf("鉴权返回设备 ID 不一致: %s vs %s", authed.ID, res.DeviceID)
	}

	// 吊销 → 令牌立即失效
	if err := svc.Revoke(ctx, res.DeviceID); err != nil {
		t.Fatalf("吊销失败: %v", err)
	}
	if _, err := svc.Authenticate(ctx, res.DeviceToken); err == nil {
		t.Fatal("吊销后设备令牌必须立即失效")
	}
	// 重复吊销 → 404（不可重复成功）
	if err := svc.Revoke(ctx, res.DeviceID); !isCode(err, apperr.CodeDeviceNotFound) {
		t.Fatalf("重复吊销应返回 device.not_found，得到 %v", err)
	}
}

// TestDevicePairCodeSingleUse 同一码二次配对失败，且与"不存在"同错误码（防枚举）。
func TestDevicePairCodeSingleUse(t *testing.T) {
	svc, _ := newDeviceTestService(t)
	ctx := context.Background()

	view, err := svc.IssuePairCode(ctx, "", model.DeviceKindMobile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Pair(ctx, "10.0.0.2", PairRequest{Code: view.Code}); err != nil {
		t.Fatalf("首次配对应成功: %v", err)
	}
	_, err = svc.Pair(ctx, "10.0.0.2", PairRequest{Code: view.Code})
	if !isCode(err, apperr.CodeDevicePairCodeInvalid) {
		t.Fatalf("二次配对应返回 device.pair_code_invalid，得到 %v", err)
	}
	// 不存在的码 → 同一错误码（无法区分，防枚举）
	_, err = svc.Pair(ctx, "10.0.0.3", PairRequest{Code: "000001"})
	if !isCode(err, apperr.CodeDevicePairCodeInvalid) {
		t.Fatalf("不存在的码应返回同一错误码，得到 %v", err)
	}
}

// TestDevicePairExpiredTTL 配对码过期（>120s）后不可用。
func TestDevicePairExpiredTTL(t *testing.T) {
	svc, _ := newDeviceTestService(t)
	ctx := context.Background()

	now := time.Now()
	svc.SetPairClock(func() time.Time { return now })
	view, err := svc.IssuePairCode(ctx, "", model.DeviceKindCLI)
	if err != nil {
		t.Fatal(err)
	}
	svc.SetPairClock(func() time.Time { return now.Add(121 * time.Second) })
	if _, err := svc.Pair(ctx, "10.0.0.4", PairRequest{Code: view.Code}); !isCode(err, apperr.CodeDevicePairCodeInvalid) {
		t.Fatalf("过期码应返回 device.pair_code_invalid，得到 %v", err)
	}
}

// TestDevicePairFailureLimiter 连续失败达上限后 429（防暴力枚举）。
func TestDevicePairFailureLimiter(t *testing.T) {
	svc, _ := newDeviceTestService(t)
	ctx := context.Background()

	for i := 0; i < devicePairAttemptLimit; i++ {
		if _, err := svc.Pair(ctx, "10.0.0.9", PairRequest{Code: "000002"}); !isCode(err, apperr.CodeDevicePairCodeInvalid) {
			t.Fatalf("第 %d 次失败应返回 device.pair_code_invalid，得到 %v", i+1, err)
		}
	}
	if _, err := svc.Pair(ctx, "10.0.0.9", PairRequest{Code: "000002"}); !isCode(err, apperr.CodeRateLimited) {
		t.Fatalf("超限后应返回 rate_limited，得到 %v", err)
	}
	// 其他来源不受影响
	if _, err := svc.Pair(ctx, "10.0.0.10", PairRequest{Code: "000002"}); !isCode(err, apperr.CodeDevicePairCodeInvalid) {
		t.Fatalf("其他来源不应被限流，得到 %v", err)
	}
}

// TestDeviceDisabled 设备域关闭时端点拒绝服务。
func TestDeviceDisabled(t *testing.T) {
	dir := t.TempDir()
	main, err := store.OpenMain(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()
	svc := NewDeviceService(DeviceServiceOptions{Main: main, Enabled: false})
	if _, err := svc.IssuePairCode(context.Background(), "", ""); !isCode(err, apperr.CodeDeviceDisabled) {
		t.Fatalf("关闭时应返回 device.disabled，得到 %v", err)
	}
	if _, err := svc.Pair(context.Background(), "x", PairRequest{Code: "000003"}); !isCode(err, apperr.CodeDeviceDisabled) {
		t.Fatalf("关闭时配对应返回 device.disabled，得到 %v", err)
	}
}

// TestDeviceConfigDefaults 配置默认值对齐阶段 C 铁律（TTL 120s / 心跳 15s / 离线 45s）。
func TestDeviceConfigDefaults(t *testing.T) {
	svc := NewDeviceService(DeviceServiceOptions{Main: nil, Enabled: true})
	if svc.PairTTL() != 120*time.Second {
		t.Fatalf("默认配对 TTL 应为 120s，得到 %s", svc.PairTTL())
	}
	if svc.Heartbeat() != 15*time.Second {
		t.Fatalf("默认心跳应为 15s，得到 %s", svc.Heartbeat())
	}
	if svc.OfflineAfter() != 45*time.Second {
		t.Fatalf("默认离线阈值应为 45s，得到 %s", svc.OfflineAfter())
	}
	if svc.TaskTTL() != 600*time.Second {
		t.Fatalf("默认任务超时应为 600s，得到 %s", svc.TaskTTL())
	}
}

// isCode 判断错误是否携带指定业务错误码。
func isCode(err error, code apperr.Code) bool {
	if err == nil {
		return false
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Code == code
}
