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

// testOwnerUserID 生成配对码时的授权用户（模拟已登录账号）。
//
// 引入它的原因：配对码必须记录授权者，配对成功后服务端据此一并签发用户会话，
// 端侧才能免二次登录。测试里用固定值便于断言 PairResult.OwnerUserID 透传正确。
const testOwnerUserID = "usr_owner_0001"

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

	view, err := svc.IssuePairCode(ctx, testOwnerUserID, "办公机", model.DeviceKindDesktop)
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
	// 免二次登录的关键：授权用户必须从配对码透传到 PairResult（api 层据此签发会话）。
	if res.OwnerUserID != testOwnerUserID {
		t.Fatalf("PairResult 应透传配对码的授权用户 %q，得到 %q", testOwnerUserID, res.OwnerUserID)
	}

	// 列表：令牌摘要绝不出栈，能力项已落库
	list, err := svc.List(ctx, false)
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

	view, err := svc.IssuePairCode(ctx, testOwnerUserID, "", model.DeviceKindMobile)
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
	view, err := svc.IssuePairCode(ctx, testOwnerUserID, "", model.DeviceKindCLI)
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
	if _, err := svc.IssuePairCode(context.Background(), "", "", ""); !isCode(err, apperr.CodeDeviceDisabled) {
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

// TestDeviceRevokeSelf 端侧自我解约：只认令牌、只吊销自己、幂等、触发吊销钩子。
//
// 守的是「客户端取消绑定后服务端必须一并取消」这条链路：
// 端侧手里只有 device_token，无法用 DELETE /devices/{did}（需用户令牌）。
func TestDeviceRevokeSelf(t *testing.T) {
	svc, main := newDeviceTestService(t)
	ctx := context.Background()

	// 配对两台设备（各自持有独立令牌）
	pairOne := func(name string) *PairResult {
		view, err := svc.IssuePairCode(ctx, testOwnerUserID, name, model.DeviceKindDesktop)
		if err != nil {
			t.Fatalf("签发配对码失败: %v", err)
		}
		res, err := svc.Pair(ctx, "client-"+name, PairRequest{Code: view.Code, Name: name})
		if err != nil {
			t.Fatalf("配对失败: %v", err)
		}
		return res
	}
	self := pairOne("我的桌面端")
	other := pairOne("同事的机器")

	// 吊销钩子：模拟 cmd 装配里的 deviceState.Remove（观察面广播）
	var hooked []string
	svc.SetRevokedHook(func(_ context.Context, id string) { hooked = append(hooked, id) })
	// 断连能力：吊销后必须主动断开长连接
	var closed []string
	svc.SetDisconnector(stubDisconnector{on: func(id, reason string) {
		closed = append(closed, id+"/"+reason)
	}})

	// ① 空令牌 → 401（调用方编程错误，不是"已解绑"）
	if err := svc.RevokeSelf(ctx, "  "); !isCode(err, apperr.CodeUnauthorized) {
		t.Fatalf("空令牌应返回 unauthorized，得到 %v", err)
	}

	// ② 正常路径：吊销自己
	if err := svc.RevokeSelf(ctx, self.DeviceToken); err != nil {
		t.Fatalf("自我解约失败: %v", err)
	}
	d, err := main.Devices().Get(ctx, self.DeviceID)
	if err != nil {
		t.Fatalf("读取自己失败: %v", err)
	}
	if d.Status != model.DeviceRevoked {
		t.Fatalf("自我解约后状态应为 revoked，得到 %s", d.Status)
	}
	// 钩子与断连都必须被触发（否则管理端 SSE 不收敛、僵尸连接不断开）
	if len(hooked) != 1 || hooked[0] != self.DeviceID {
		t.Fatalf("吊销钩子应收到自己的 device_id，得到 %v", hooked)
	}
	if len(closed) != 1 || closed[0] != self.DeviceID+"/device.revoked" {
		t.Fatalf("应断开自己的长连接，得到 %v", closed)
	}

	// ③ 越权隔离：别人的设备毫发无损，令牌仍然有效
	if err := svc.RevokeSelf(ctx, other.DeviceToken); err != nil {
		t.Fatalf("第二台自我解约失败: %v", err)
	}
	od, err := main.Devices().Get(ctx, other.DeviceID)
	if err != nil || od.Status != model.DeviceRevoked {
		t.Fatalf("第二台设备应被正常吊销: %+v err=%v", od, err)
	}

	// ④ 幂等：同一令牌再来一次 → 不报错，且不再重复触发钩子/断连
	before := len(hooked)
	if err := svc.RevokeSelf(ctx, self.DeviceToken); err != nil {
		t.Fatalf("重复自我解约应幂等成功（不报错），得到 %v", err)
	}
	if len(hooked) != before {
		t.Fatalf("重复解约不应重复触发吊销钩子，钩子数 %d → %d", before, len(hooked))
	}

	// ⑤ 伪造令牌 → 幂等成功（不泄露"该令牌是否存在"）
	if err := svc.RevokeSelf(ctx, "hgd_forged_token_value"); err != nil {
		t.Fatalf("伪造令牌应幂等成功（防枚举），得到 %v", err)
	}

	// ⑥ 列表投影：两台都已解绑 → 默认列表为空；审计视角仍能看到 2 条
	visible, err := svc.List(ctx, false)
	if err != nil {
		t.Fatalf("列出设备失败: %v", err)
	}
	if len(visible) != 0 {
		t.Fatalf("全部解绑后默认列表应为空（解绑即消失），得到 %d 条", len(visible))
	}
	all, err := svc.List(ctx, true)
	if err != nil {
		t.Fatalf("列出全部设备失败: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("审计视角应含 2 条已吊销设备，得到 %d 条", len(all))
	}
	for _, d := range all {
		if d.TokenHash != "" {
			t.Fatal("审计视角同样绝不允许回传令牌摘要")
		}
	}
}

// TestDeviceRevokeNotifiesHook 管理端吊销（按 ID）同样必须触发观察面钩子。
//
// 否则 Web 端点了「解除绑定」后，SSE 收不到 device.revoked，列表只能等
// 用户手动点刷新才消失 —— 那正是用户报告的「取消绑定后设备没消失」。
func TestDeviceRevokeNotifiesHook(t *testing.T) {
	svc, _ := newDeviceTestService(t)
	ctx := context.Background()

	view, err := svc.IssuePairCode(ctx, testOwnerUserID, "待解绑设备", model.DeviceKindDesktop)
	if err != nil {
		t.Fatalf("签发配对码失败: %v", err)
	}
	res, err := svc.Pair(ctx, "client-x", PairRequest{Code: view.Code})
	if err != nil {
		t.Fatalf("配对失败: %v", err)
	}

	var hooked []string
	svc.SetRevokedHook(func(_ context.Context, id string) { hooked = append(hooked, id) })

	if err := svc.Revoke(ctx, res.DeviceID); err != nil {
		t.Fatalf("吊销失败: %v", err)
	}
	if len(hooked) != 1 || hooked[0] != res.DeviceID {
		t.Fatalf("管理端吊销也必须触发吊销钩子（供 SSE 广播），得到 %v", hooked)
	}

	// 未装配钩子时不得 panic（设备域关闭 / 单测环境）
	svc2, _ := newDeviceTestService(t)
	svc2.SetRevokedHook(nil)
	view2, err := svc2.IssuePairCode(ctx, testOwnerUserID, "无钩子设备", model.DeviceKindDesktop)
	if err != nil {
		t.Fatalf("签发配对码失败: %v", err)
	}
	res2, err := svc2.Pair(ctx, "client-y", PairRequest{Code: view2.Code})
	if err != nil {
		t.Fatalf("配对失败: %v", err)
	}
	if err := svc2.Revoke(ctx, res2.DeviceID); err != nil {
		t.Fatalf("未装配钩子时吊销不应失败: %v", err)
	}
}

// stubDisconnector 记录断连调用的测试替身。
type stubDisconnector struct {
	on func(deviceID, reason string)
}

func (s stubDisconnector) CloseDevice(deviceID, reason string) {
	if s.on != nil {
		s.on(deviceID, reason)
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
