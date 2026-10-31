package device

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"harness-go/internal/apperr"
	"harness-go/internal/model"
)

// ============ 协议信封与版本协商 ============

func TestProtocolEnvelopeShapeAndVersion(t *testing.T) {
	env := MustEnvelope(MsgPing, "f-1", PingPayload{CapabilitiesHash: "abc", ProtocolVersion: 1})
	if env.V != CurrentProtocolVersion || env.Type != MsgPing || env.ID != "f-1" {
		t.Fatalf("信封字段错误: %+v", env)
	}
	if env.TS == 0 {
		t.Fatal("信封应带毫秒时间戳")
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	// 信封外层键名属冻结契约：v / type / id / payload
	for _, k := range []string{`"v":`, `"type":"ping"`, `"id":"f-1"`, `"payload":`} {
		if !strings.Contains(string(raw), k) {
			t.Fatalf("信封缺少键 %s: %s", k, raw)
		}
	}

	var back Envelope
	if uErr := json.Unmarshal(raw, &back); uErr != nil {
		t.Fatal(uErr)
	}
	var p PingPayload
	if dErr := back.Decode(&p); dErr != nil {
		t.Fatal(dErr)
	}
	if p.CapabilitiesHash != "abc" || p.ProtocolVersion != 1 {
		t.Fatalf("负载解码错误: %+v", p)
	}
}

func TestEnvelopeValidate(t *testing.T) {
	if err := (Envelope{Type: ""}).Validate(); err == nil {
		t.Fatal("缺 type 应报错")
	}
	if err := (Envelope{Type: "nope"}).Validate(); err == nil {
		t.Fatal("未知类型应报错")
	}
	for _, ty := range []MessageType{MsgHello, MsgWelcome, MsgPing, MsgPong, MsgError,
		MsgTaskDispatch, MsgTaskProgress, MsgTaskResult, MsgLeaseRevoked} {
		if err := (Envelope{Type: ty}).Validate(); err != nil {
			t.Fatalf("%s 应为合法类型: %v", ty, err)
		}
	}
}

func TestCompatibleVersion(t *testing.T) {
	// 未声明版本 → 兼容（老端）
	if err := CompatibleVersion(0); err != nil {
		t.Fatalf("v=0 应兼容: %v", err)
	}
	if err := CompatibleVersion(CurrentProtocolVersion); err != nil {
		t.Fatalf("当前版本应兼容: %v", err)
	}
	// 过高版本 → protocol_mismatch（不得静默降级）
	ae := apperr.From(CompatibleVersion(CurrentProtocolVersion + 1))
	if ae.Code != apperr.CodeDeviceProtocolMismatch {
		t.Fatalf("过高版本应为 protocol_mismatch，得到 %s", ae.Code)
	}
	if ae.Status() != http.StatusConflict {
		t.Fatalf("protocol_mismatch 应为 409，得到 %d", ae.Status())
	}
	vers := SupportedVersions()
	if len(vers) == 0 || vers[0] != CurrentProtocolVersion {
		t.Fatalf("支持版本集合错误: %v", vers)
	}
}

func TestTokenFromRequest(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/devices/ws?token=hgd_abc", nil)
	if got := TokenFromRequest(r); got != "hgd_abc" {
		t.Fatalf("query token 提取失败: %q", got)
	}
	r2 := httptest.NewRequest(http.MethodGet, "/api/v1/devices/ws", nil)
	r2.Header.Set("Authorization", "Bearer hgd_xyz")
	if got := TokenFromRequest(r2); got != "hgd_xyz" {
		t.Fatalf("Bearer token 提取失败: %q", got)
	}
	r3 := httptest.NewRequest(http.MethodGet, "/api/v1/devices/ws", nil)
	if got := TokenFromRequest(r3); got != "" {
		t.Fatalf("无令牌应返回空，得到 %q", got)
	}
}

// ============ Hub：注册 / 替换 / 超时判离线 ============

type fakeConn struct {
	id     string
	closes []string
	seen   time.Time

	mu sync.Mutex
}

func (c *fakeConn) DeviceID() string { return c.id }
func (c *fakeConn) Send(Envelope) error {
	return nil
}
func (c *fakeConn) CloseWith(code, _ string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes = append(c.closes, code)
}
func (c *fakeConn) LastSeen() time.Time { return c.seen }
func (c *fakeConn) RemoteAddr() string  { return "127.0.0.1:9999" }

func (c *fakeConn) closeCodes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.closes))
	copy(out, c.closes)
	return out
}

func TestHubRegisterReplaceAndOfflineSweep(t *testing.T) {
	var online, offline, beats int
	h := NewHub(HubOptions{
		OfflineAfter: 45 * time.Second,
		OnOnline:     func(context.Context, string) { online++ },
		OnOffline:    func(context.Context, string) { offline++ },
		OnHeartbeat:  func(context.Context, string, int64, json.RawMessage, string, int) { beats++ },
	})
	now := time.Unix(1_700_000_000, 0)
	h.SetClock(func() time.Time { return now })

	c1 := &fakeConn{id: "dev-1", seen: now}
	if replaced := h.Register(c1); replaced {
		t.Fatal("首次注册不应为替换")
	}
	if !h.Online("dev-1") || h.Count() != 1 {
		t.Fatalf("注册后应在线: count=%d", h.Count())
	}

	// 同设备新连接 → 旧连接收到 device.replaced
	c2 := &fakeConn{id: "dev-1", seen: now}
	if replaced := h.Register(c2); !replaced {
		t.Fatal("同设备二次注册应判定替换")
	}
	if codes := c1.closeCodes(); len(codes) != 1 || codes[0] != FrameCodeReplaced {
		t.Fatalf("旧连接应收到 %s，得到 %v", FrameCodeReplaced, codes)
	}

	// 旧连接 Unregister 不得删除新连接（关键防误删语义）
	if h.Unregister(c1) {
		t.Fatal("旧连接注销应返回 false（当前连接已是新连接）")
	}
	if !h.Online("dev-1") {
		t.Fatal("旧连接注销后新连接应仍在线")
	}

	// 心跳刷新
	h.Beat("dev-1", json.RawMessage(`{"cpu":12}`), "hash1", 1)
	if beats != 1 {
		t.Fatalf("心跳回调应被调用 1 次，得到 %d", beats)
	}

	// 30s 未超阈值 → 不判离线
	now = now.Add(30 * time.Second)
	if n := h.Sweep(); n != 0 {
		t.Fatalf("30s 不应判离线，得到 %d", n)
	}

	// 46s（>45s）→ 判离线并关闭连接
	now = now.Add(46 * time.Second)
	if n := h.Sweep(); n != 1 {
		t.Fatalf("超过 offline_after 应判离线 1 台，得到 %d", n)
	}
	if codes := c2.closeCodes(); len(codes) != 1 || codes[0] != FrameCodeOfflineTimeout {
		t.Fatalf("超时连接应收到 %s，得到 %v", FrameCodeOfflineTimeout, codes)
	}
	if h.Online("dev-1") || h.Count() != 0 {
		t.Fatal("判离线后应无在线连接")
	}
	if online != 2 || offline != 1 {
		t.Fatalf("回调次数不符 online=%d offline=%d", online, offline)
	}
}

func TestHubCloseDeviceDoesNotMarkOffline(t *testing.T) {
	offline := 0
	h := NewHub(HubOptions{
		OfflineAfter: time.Second,
		OnOffline:    func(context.Context, string) { offline++ },
	})
	c := &fakeConn{id: "dev-9", seen: time.Now()}
	h.Register(c)

	// 吊销场景：只断连，不回调 OnOffline（否则会覆盖 revoked 终态）
	h.CloseDevice("dev-9", "device.revoked")
	if offline != 0 {
		t.Fatalf("CloseDevice 不应回调 OnOffline，得到 %d", offline)
	}
	if codes := c.closeCodes(); len(codes) != 1 || codes[0] != FrameCodeRevoked {
		t.Fatalf("应收到 %s，得到 %v", FrameCodeRevoked, codes)
	}
	if h.Online("dev-9") {
		t.Fatal("断开后不应在线")
	}
}

func TestHubCloseAllIdempotent(t *testing.T) {
	h := NewHub(HubOptions{OfflineAfter: time.Second})
	h.Start(context.Background())
	c := &fakeConn{id: "dev-a", seen: time.Now()}
	h.Register(c)
	h.CloseAll("测试关闭")
	h.CloseAll("重复关闭")
	if codes := c.closeCodes(); len(codes) != 1 {
		t.Fatalf("CloseAll 应只关闭一次，得到 %v", codes)
	}
	if h.Count() != 0 {
		t.Fatalf("CloseAll 后在线数应为 0，得到 %d", h.Count())
	}
	// 关闭后注册应被拒绝并立即关闭
	c2 := &fakeConn{id: "dev-b", seen: time.Now()}
	h.Register(c2)
	if codes := c2.closeCodes(); len(codes) != 1 || codes[0] != FrameCodeServerClosing {
		t.Fatalf("关闭后注册应收到 %s，得到 %v", FrameCodeServerClosing, codes)
	}
}

// ============ 网关：鉴权在升级前 + 读写循环 ============

// newGatewayServer 组装带真实 WS 的测试服务器（hub 不注入落库回调，纯内存态）。
func newGatewayServer(t *testing.T, opts GatewayOptions) (*httptest.Server, *Hub, *Gateway) {
	t.Helper()
	hub := NewHub(HubOptions{OfflineAfter: opts.OfflineAfter})
	opts.Hub = hub
	opts.Heartbeat = 15 * time.Second
	gw := NewGateway(opts)
	srv := httptest.NewServer(http.HandlerFunc(gw.ServeHTTP))
	t.Cleanup(func() {
		srv.Close()
		hub.CloseAll("测试结束")
	})
	return srv, hub, gw
}

func wsURL(serverURL, token string) string {
	return "ws" + strings.TrimPrefix(serverURL, "http") + "/?token=" + token
}

func TestGatewayRejectsBeforeUpgrade(t *testing.T) {
	authErr := apperr.New(apperr.CodeUnauthorized, "设备令牌无效")
	srv, hub, _ := newGatewayServer(t, GatewayOptions{
		Authenticate: func(_ context.Context, token string) (*model.Device, error) {
			if token == "hgd_good" {
				return &model.Device{ID: "dev-1", Name: "本机", Status: model.DeviceOffline}, nil
			}
			return nil, authErr
		},
	})

	// ① 无令牌 → 401（普通 HTTP，非 101）
	resp, err := http.Get(srv.URL) //nolint:noctx // 测试用
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401，得到 %d", resp.StatusCode)
	}
	if resp.Header.Get("Upgrade") != "" {
		t.Fatal("鉴权失败不得进入升级流程")
	}

	// ② 错误令牌 → 401 且无连接注册
	resp2, err := http.Get(srv.URL + "/?token=hgd_bad") //nolint:noctx
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误令牌应 401，得到 %d", resp2.StatusCode)
	}
	var body apperr.Body
	if dErr := json.NewDecoder(resp2.Body).Decode(&body); dErr != nil {
		t.Fatal(dErr)
	}
	if body.Error.Code != string(apperr.CodeUnauthorized) {
		t.Fatalf("错误码应为 unauthorized，得到 %s", body.Error.Code)
	}
	if hub.Count() != 0 {
		t.Fatalf("鉴权失败不得注册连接，在线数 %d", hub.Count())
	}
}

func TestGatewayPingPongAndWelcome(t *testing.T) {
	beats := make(chan int, 4)
	onlineCh := make(chan struct{}, 4)
	hub := NewHub(HubOptions{
		OfflineAfter: 45 * time.Second,
		OnOnline:     func(context.Context, string) { onlineCh <- struct{}{} },
		OnHeartbeat: func(_ context.Context, id string, _ int64, _ json.RawMessage, _ string, proto int) {
			beats <- proto
		},
	})
	gw := NewGateway(GatewayOptions{
		Hub: hub,
		Authenticate: func(context.Context, string) (*model.Device, error) {
			return &model.Device{ID: "dev-1", Name: "本机", Status: model.DeviceOffline}, nil
		},
		Heartbeat:    15 * time.Second,
		OfflineAfter: 45 * time.Second,
	})
	srv := httptest.NewServer(http.HandlerFunc(gw.ServeHTTP))
	defer func() {
		srv.Close()
		hub.CloseAll("测试结束")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL(srv.URL, "hgd_good"), nil)
	if err != nil {
		t.Fatalf("WS 拨号失败: %v", err)
	}
	defer ws.CloseNow()

	// ① 首帧必须是 welcome（携带心跳与离线阈值）
	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("读取 welcome 失败: %v", err)
	}
	var welcome Envelope
	if uErr := json.Unmarshal(data, &welcome); uErr != nil {
		t.Fatal(uErr)
	}
	if welcome.Type != MsgWelcome {
		t.Fatalf("首帧应为 welcome，得到 %s", welcome.Type)
	}
	var wp WelcomePayload
	if dErr := welcome.Decode(&wp); dErr != nil {
		t.Fatal(dErr)
	}
	if wp.DeviceID != "dev-1" || wp.HeartbeatS != 15 || wp.OfflineAfterS != 45 ||
		wp.ProtocolVersion != CurrentProtocolVersion {
		t.Fatalf("welcome 负载错误: %+v", wp)
	}

	select {
	case <-onlineCh:
	case <-time.After(2 * time.Second):
		t.Fatal("连接注册应触发 OnOnline 回调")
	}

	// ② ping → pong，并刷新心跳落库
	ping := MustEnvelope(MsgPing, "p-1", PingPayload{
		Load:             json.RawMessage(`{"cpu":7}`),
		CapabilitiesHash: "hash-a",
		ProtocolVersion:  CurrentProtocolVersion,
	})
	raw, _ := json.Marshal(ping)
	if wErr := ws.Write(ctx, websocket.MessageText, raw); wErr != nil {
		t.Fatalf("发送 ping 失败: %v", wErr)
	}
	_, data2, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("读取 pong 失败: %v", err)
	}
	var pong Envelope
	_ = json.Unmarshal(data2, &pong)
	if pong.Type != MsgPong || pong.ID != "p-1" {
		t.Fatalf("应答应为 pong(id=p-1)，得到 %s(id=%s)", pong.Type, pong.ID)
	}
	select {
	case proto := <-beats:
		if proto != CurrentProtocolVersion {
			t.Fatalf("心跳应带协议版本 %d，得到 %d", CurrentProtocolVersion, proto)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ping 应触发心跳落库回调")
	}

	// ③ 未知帧类型 → error{device.bad_frame}，连接保持
	bad := []byte(`{"v":1,"type":"nope"}`)
	if wErr := ws.Write(ctx, websocket.MessageText, bad); wErr != nil {
		t.Fatal(wErr)
	}
	_, data3, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("读取 bad_frame 应答失败: %v", err)
	}
	var badAck Envelope
	_ = json.Unmarshal(data3, &badAck)
	if badAck.Type != MsgError {
		t.Fatalf("非法帧应回 error，得到 %s", badAck.Type)
	}
	var ep ErrorPayload
	_ = badAck.Decode(&ep)
	if ep.Code != FrameCodeBadFrame || ep.Fatal {
		t.Fatalf("非法帧错误码应为 %s 且非 fatal，得到 %+v", FrameCodeBadFrame, ep)
	}

	// ④ 断线后 hub 应清理（Unregister）→ 离线回调
	ws.CloseNow()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.Count() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("断线后应清理在线连接，当前 %d", hub.Count())
}

func TestGatewayRejectsIncompatibleProtocol(t *testing.T) {
	hub := NewHub(HubOptions{OfflineAfter: 45 * time.Second})
	gw := NewGateway(GatewayOptions{
		Hub: hub,
		Authenticate: func(context.Context, string) (*model.Device, error) {
			return &model.Device{ID: "dev-x"}, nil
		},
		Heartbeat:    15 * time.Second,
		OfflineAfter: 45 * time.Second,
	})
	srv := httptest.NewServer(http.HandlerFunc(gw.ServeHTTP))
	defer func() {
		srv.Close()
		hub.CloseAll("测试结束")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// query 声明一个过高的协议版本
	url := wsURL(srv.URL, "hgd_good") + "&protocol_version=99"
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("WS 拨号失败: %v", err)
	}
	defer ws.CloseNow()

	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("应收到 protocol_mismatch 错误帧: %v", err)
	}
	var env Envelope
	_ = json.Unmarshal(data, &env)
	if env.Type != MsgError {
		t.Fatalf("应为 error 帧，得到 %s", env.Type)
	}
	var ep ErrorPayload
	_ = env.Decode(&ep)
	if ep.Code != FrameCodeProtocolMismatch || !ep.Fatal {
		t.Fatalf("应为 %s 且 fatal，得到 %+v", FrameCodeProtocolMismatch, ep)
	}
	// 随后连接被关闭
	if _, _, rErr := ws.Read(ctx); rErr == nil {
		t.Fatal("协议不兼容后连接应被关闭")
	}
	if hub.Count() != 0 {
		t.Fatalf("协议不兼容的连接不应留在在线表，得到 %d", hub.Count())
	}
}

func TestGatewayTaskFrameWithoutHandler(t *testing.T) {
	hub := NewHub(HubOptions{OfflineAfter: 45 * time.Second})
	gw := NewGateway(GatewayOptions{
		Hub: hub,
		Authenticate: func(context.Context, string) (*model.Device, error) {
			return &model.Device{ID: "dev-t"}, nil
		},
		Heartbeat:    15 * time.Second,
		OfflineAfter: 45 * time.Second,
		// Handler 为 nil：C-4 之前任务帧应回 device.task_unsupported，且不断连
	})
	srv := httptest.NewServer(http.HandlerFunc(gw.ServeHTTP))
	defer func() {
		srv.Close()
		hub.CloseAll("测试结束")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL(srv.URL, "hgd_t"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if _, _, rErr := ws.Read(ctx); rErr != nil { // welcome
		t.Fatal(rErr)
	}

	res := MustEnvelope(MsgTaskResult, "evt-1", TaskResultPayload{TaskID: "evt-1", Status: "success"})
	raw, _ := json.Marshal(res)
	if wErr := ws.Write(ctx, websocket.MessageText, raw); wErr != nil {
		t.Fatal(wErr)
	}
	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("读取应答失败: %v", err)
	}
	var env Envelope
	_ = json.Unmarshal(data, &env)
	var ep ErrorPayload
	_ = env.Decode(&ep)
	if env.Type != MsgError || ep.Code != string(apperr.CodeToolUnavailable) {
		t.Fatalf("应为 tool.unavailable 错误帧，得到 %s/%s", env.Type, ep.Code)
	}
	if ep.Fatal {
		t.Fatal("task 帧不支持时不应断连（便于降级后重派）")
	}
}

// TestGatewayReconnectAfterNetworkDrop 拔网 30s 内重连收敛：
// 断开 → hub 标 offline → 重连后同一设备重新在线且 welcome 再次下发。
func TestGatewayReconnectAfterNetworkDrop(t *testing.T) {
	var mu sync.Mutex
	var onlineCount, offlineCount int
	hub := NewHub(HubOptions{
		OfflineAfter: 45 * time.Second,
		OnOnline:     func(context.Context, string) { mu.Lock(); onlineCount++; mu.Unlock() },
		OnOffline:    func(context.Context, string) { mu.Lock(); offlineCount++; mu.Unlock() },
	})
	gw := NewGateway(GatewayOptions{
		Hub: hub,
		Authenticate: func(context.Context, string) (*model.Device, error) {
			return &model.Device{ID: "dev-r"}, nil
		},
		Heartbeat:    15 * time.Second,
		OfflineAfter: 45 * time.Second,
	})
	srv := httptest.NewServer(http.HandlerFunc(gw.ServeHTTP))
	defer func() {
		srv.Close()
		hub.CloseAll("测试结束")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dialAndWelcome := func() *websocket.Conn {
		ws, _, err := websocket.Dial(ctx, wsURL(srv.URL, "hgd_r"), nil)
		if err != nil {
			t.Fatalf("拨号失败: %v", err)
		}
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("读取 welcome 失败: %v", err)
		}
		var env Envelope
		_ = json.Unmarshal(data, &env)
		if env.Type != MsgWelcome {
			t.Fatalf("应为 welcome，得到 %s", env.Type)
		}
		return ws
	}

	ws1 := dialAndWelcome()
	ws1.CloseNow()
	waitFor(t, func() bool { return hub.Count() == 0 })

	ws2 := dialAndWelcome()
	defer ws2.CloseNow()
	if !hub.Online("dev-r") {
		t.Fatal("重连后应重新在线")
	}
	mu.Lock()
	defer mu.Unlock()
	if onlineCount != 2 || offlineCount != 1 {
		t.Fatalf("重连收敛次数不符 online=%d offline=%d", onlineCount, offlineCount)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}
