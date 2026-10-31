package device

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"harness-go/internal/apperr"
	"harness-go/internal/model"
)

// ============ C-9 出口：单测试覆盖整条多端协同主链 ============
//
// 口径（对齐 多端协同 §9 验收矩阵 与 开发实施计划 阶段 C 出口门）：
//
//	① 配对   6 位码签发 → 端侧兑换 → 设备令牌（**一次性**，二次兑换必失败且错误不可区分）
//	② 连上   device_token 在**升级前**鉴权 → welcome（心跳/离线阈值/协议版本）
//	③ 派发   Dispatch → 端侧收 task.dispatch（task_id 恒等于 tool_event_id）
//	④ 回流   progress 不落库、result 收敛为 ok 且 source_trust 强制 untrusted
//	⑤ 降级   端侧拔网 → hub 标 offline → Dispatch **立即** Degraded（不等 deadline，无用户可见错误）
//	⑥ 重连   同一设备令牌重连 → welcome 再次下发 → 设备仍为同一台 → 派发恢复成功
//
// 这是「网关 + 在线表 + 派发器 + 配对码」四者的**装配级**断言：
// 单点能力各自有专测（gateway_test / dispatcher_test / paircode_test），本测试只保证拼起来不脱节。

// chainDeviceStore 内存设备表（token 明文 → 设备），替代主库 devices 仓储。
type chainDeviceStore struct {
	mu   sync.Mutex
	byID map[string]*model.Device
	byTH map[string]*model.Device
}

func newChainDeviceStore() *chainDeviceStore {
	return &chainDeviceStore{byID: map[string]*model.Device{}, byTH: map[string]*model.Device{}}
}

// put 登记设备（按令牌摘要索引），模拟配对成功后的落库结果。
func (s *chainDeviceStore) put(dev *model.Device, plainToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dev.TokenHash = HashDeviceToken(plainToken)
	if dev.Status == "" {
		dev.Status = model.DeviceOffline
	}
	s.byID[dev.ID] = dev
	s.byTH[dev.TokenHash] = dev
}

// authenticate 升级前鉴权（等价 service.DeviceService.Authenticate 的语义）。
func (s *chainDeviceStore) authenticate(_ context.Context, token string) (*model.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dev, ok := s.byTH[HashDeviceToken(token)]
	if !ok || dev.Status == model.DeviceRevoked {
		return nil, apperr.New(apperr.CodeUnauthorized, "设备令牌无效或已吊销")
	}
	clone := *dev
	return &clone, nil
}

func TestGatewayFullChainPairDispatchDegradeReconnect(t *testing.T) {
	devices := newChainDeviceStore()

	// ① 配对：签发 → 兑换（一次性）→ 设备令牌
	issuer := NewPairCodeIssuer(newFakePairCodeStore(), DefaultPairTTL)
	if issuer.TTL() != 120*time.Second {
		t.Fatalf("配对码 TTL 铁律 120s，得到 %s", issuer.TTL())
	}
	code, err := issuer.Issue(context.Background(), "桌面端", model.DeviceKindDesktop)
	if err != nil {
		t.Fatalf("签发配对码失败: %v", err)
	}
	if !ValidPairCode(code.Code) {
		t.Fatalf("配对码应为 6 位纯数字，得到 %q", code.Code)
	}
	redeemed, err := issuer.Redeem(context.Background(), code.Code)
	if err != nil {
		t.Fatalf("兑换配对码失败: %v", err)
	}
	if redeemed.DeviceName != "桌面端" || redeemed.DeviceKind != model.DeviceKindDesktop {
		t.Fatalf("兑换结果应沿用预置 name/kind: %+v", redeemed)
	}
	// 一次性消费 + 防枚举：二次兑换与「不存在的码」返回同一错误
	_, errAgain := issuer.Redeem(context.Background(), code.Code)
	if !errors.Is(errAgain, ErrPairCodeInvalid) {
		t.Fatalf("配对码只能消费一次，得到 %v", errAgain)
	}
	_, ghost := issuer.Redeem(context.Background(), "000000")
	if !errors.Is(ghost, ErrPairCodeInvalid) || ghost.Error() != errAgain.Error() {
		t.Fatalf("已消费与不存在必须不可区分: consumed=%v ghost=%v", errAgain, ghost)
	}

	token, err := NewDeviceToken()
	if err != nil {
		t.Fatalf("生成设备令牌失败: %v", err)
	}
	const deviceID = "dev-chain-1"
	devices.put(&model.Device{ID: deviceID, Name: "桌面端", Kind: model.DeviceKindDesktop}, token)

	// ② 装配：hub + dispatcher + gateway（Handler 接派发器，PendingCount 接 pong.pending）
	var mu sync.Mutex
	var onlineSeq, offlineSeq int
	hub := NewHub(HubOptions{
		OfflineAfter: 45 * time.Second,
		OnOnline:     func(context.Context, string) { mu.Lock(); onlineSeq++; mu.Unlock() },
		OnOffline:    func(context.Context, string) { mu.Lock(); offlineSeq++; mu.Unlock() },
	})
	dispatcher := NewDispatcher(DispatcherOptions{Hub: hub, TaskTTL: 2 * time.Second})
	t.Cleanup(func() { dispatcher.Close("测试结束") })
	gw := NewGateway(GatewayOptions{
		Hub:          hub,
		Authenticate: devices.authenticate,
		Heartbeat:    15 * time.Second,
		OfflineAfter: 45 * time.Second,
		Handler:      dispatcher.HandleFrame,
		PendingCount: dispatcher.PendingCount,
	})
	srv := httptest.NewServer(http.HandlerFunc(gw.ServeHTTP))
	defer func() {
		srv.Close()
		hub.CloseAll("测试结束")
	}()

	// 升级前鉴权：错误令牌 → 401（绝不 101）
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, resp, dErr := websocket.Dial(ctx, wsURL(srv.URL, "hgd_not_exists"), nil); dErr == nil {
		t.Fatal("错误设备令牌不应升级成功")
	} else if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误令牌应返回 401，得到 %+v", resp)
	}

	// ③ 连上（用配对换来的令牌）→ welcome
	dial := func() *websocket.Conn {
		ws, _, dErr := websocket.Dial(ctx, wsURL(srv.URL, token), nil)
		if dErr != nil {
			t.Fatalf("WS 升级失败（检查 statusRecorder/Unwrap）: %v", dErr)
		}
		env := readEnvelope(t, ctx, ws)
		if env.Type != MsgWelcome {
			t.Fatalf("首帧应为 welcome，得到 %s", env.Type)
		}
		var wp WelcomePayload
		if uErr := env.Decode(&wp); uErr != nil {
			t.Fatal(uErr)
		}
		if wp.DeviceID != deviceID || wp.HeartbeatS != 15 || wp.OfflineAfterS != 45 ||
			wp.ProtocolVersion != CurrentProtocolVersion {
			t.Fatalf("welcome 负载错误: %+v", wp)
		}
		waitFor(t, func() bool { return hub.Online(deviceID) })
		return ws
	}
	ws := dial()

	// ④ 派发 + 回流（task.dispatch → progress → result）
	res, err := dispatchWithRunner(t, ctx, ws, dispatcher, "evt-chain-1")
	if err != nil {
		t.Fatalf("端侧派发应成功: %v", err)
	}
	if res.Status != model.ToolOK || res.DeviceID != deviceID || res.Degraded || res.Timeout {
		t.Fatalf("回流收敛结果错误: %+v", res)
	}
	if res.SourceTrust != model.TrustUntrusted {
		t.Fatalf("端侧结果必须 untrusted（不变量 4），得到 %s", res.SourceTrust)
	}

	// ⑤ 拔网 → hub 立即判离线 → 派发**立即降级**（不变量 5，无用户可见错误）
	ws.CloseNow()
	waitFor(t, func() bool { return hub.Count() == 0 })
	degraded, dErr := dispatcher.Dispatch(ctx, TaskRequest{
		ToolEventID: "evt-chain-offline", ToolName: "fs.read", Timeout: 1500 * time.Millisecond,
	})
	if !errors.Is(dErr, ErrDeviceUnavailable) {
		t.Fatalf("无在线设备应返回 ErrDeviceUnavailable，得到 %v", dErr)
	}
	if !degraded.Degraded || degraded.Error == "" {
		t.Fatalf("降级结果必须带 Degraded=true 与原因: %+v", degraded)
	}
	if degraded.Status == model.ToolOK {
		t.Fatalf("降级不得伪装成功（应由执行面转本地 server）: %+v", degraded)
	}

	// ⑥ 拔网重连：同一设备令牌 → welcome 再次下发 → 仍是同一台设备 → 派发恢复
	ws2 := dial()
	defer ws2.CloseNow()
	mu.Lock()
	gotOnline, gotOffline := onlineSeq, offlineSeq
	mu.Unlock()
	if gotOnline != 2 || gotOffline != 1 {
		t.Fatalf("重连收敛次数不符 online=%d offline=%d", gotOnline, gotOffline)
	}
	res2, err := dispatchWithRunner(t, ctx, ws2, dispatcher, "evt-chain-2")
	if err != nil {
		t.Fatalf("重连后派发应成功: %v", err)
	}
	if res2.DeviceID != deviceID || res2.Status != model.ToolOK {
		t.Fatalf("重连后回流应归属同一设备: %+v", res2)
	}
}

// dispatchWithRunner 发起一次派发，并由「假 Runner」显式回 progress + result 两帧。
func dispatchWithRunner(t *testing.T, ctx context.Context, ws *websocket.Conn,
	d *Dispatcher, toolEventID string) (TaskResult, error) {
	t.Helper()
	done := make(chan TaskResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := d.Dispatch(ctx, TaskRequest{
			RunID: "run-chain", ToolEventID: toolEventID, ToolName: "fs.read",
			Args: json.RawMessage(`{"path":"a.txt"}`),
		})
		if err != nil {
			errCh <- err
			return
		}
		done <- res
	}()

	_, p := awaitDispatch(t, ctx, ws)
	if p.TaskID != toolEventID || p.ToolEventID != toolEventID {
		t.Fatalf("task_id 必须等于 tool_event_id，得到 %q / %q", p.TaskID, p.ToolEventID)
	}
	sendFrame(t, ctx, ws, MustEnvelope(MsgTaskProgress, p.TaskID, TaskProgressPayload{
		TaskID: p.TaskID, RunID: p.RunID, ToolEventID: p.ToolEventID, Chunk: "执行中 50%",
	}))
	resultEnv := MustEnvelope(MsgTaskResult, p.TaskID, TaskResultPayload{
		TaskID: p.TaskID, RunID: p.RunID, ToolEventID: p.ToolEventID,
		Status: TaskStatusSuccess, Result: json.RawMessage(`{"content":"chain ok"}`),
		SourceTrust: string(model.TrustTrusted), // 端侧谎称可信，服务端必须覆盖
	})
	sendFrame(t, ctx, ws, resultEnv)

	select {
	case res := <-done:
		return res, nil
	case err := <-errCh:
		return TaskResult{}, err
	case <-time.After(5 * time.Second):
		t.Fatal("派发未在 5s 内收敛")
		return TaskResult{}, nil
	}
}
