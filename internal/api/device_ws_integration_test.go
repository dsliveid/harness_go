package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"harness-go/internal/device"
	"harness-go/internal/observ"
)

// TestDeviceWSEndToEnd 阶段 C-3 出口：走**真实路由 + 真实中间件链**的 WS 端到端。
//
// 断言要点：
//   - 配对 → 用返回的 device_token 走 /api/v1/devices/ws?token=... 升级成功
//   - 首帧 welcome 携带 heartbeat_s / offline_after_s / protocol_version
//   - ping → pong 往返；ping 的 load/capabilities_hash 落库（devices 表可见）
//   - 无令牌/错误令牌在**升级前**返回 401（绝不 101）
//   - 升级经过 AccessLog 中间件的 statusRecorder 仍可用（Unwrap 生效，否则 501）
//   - 吊销设备后长连接被主动断开（disconnector 接线）
func TestDeviceWSEndToEnd(t *testing.T) {
	srv, token := newTestServer(t)

	// ① 配对（免登录）
	var code struct {
		Code string `json:"code"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/pair-codes", token,
		map[string]any{"name": "桌面端"}, http.StatusCreated, &code)
	var paired struct {
		DeviceID    string `json:"device_id"`
		DeviceToken string `json:"device_token"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/devices/pair", "",
		map[string]any{"code": code.Code, "name": "桌面端", "kind": "desktop",
			"capabilities": []map[string]string{{"name": "fs.read", "version": "1.0"}},
		}, http.StatusCreated, &paired)

	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/devices/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// ② 无令牌 → 401（升级前完成，非 101）
	if _, resp, err := websocket.Dial(ctx, wsBase, nil); err == nil {
		t.Fatal("无令牌应升级失败")
	} else if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无令牌应返回 401，得到 %+v", resp)
	}

	// ③ 错误令牌 → 401
	if _, resp, err := websocket.Dial(ctx, wsBase+"?token=hgd_not_exist", nil); err == nil {
		t.Fatal("错误令牌应升级失败")
	} else if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误令牌应返回 401，得到 %+v", resp)
	}

	// ④ 正确令牌 → 升级成功（证明 statusRecorder.Unwrap 生效，否则 501）
	ws, _, err := websocket.Dial(ctx, wsBase+"?token="+paired.DeviceToken, nil)
	if err != nil {
		t.Fatalf("WS 升级失败（检查 statusRecorder.Unwrap）: %v", err)
	}
	defer ws.CloseNow()

	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("读取 welcome 失败: %v", err)
	}
	var welcome device.Envelope
	if uErr := json.Unmarshal(data, &welcome); uErr != nil {
		t.Fatal(uErr)
	}
	if welcome.Type != device.MsgWelcome {
		t.Fatalf("首帧应为 welcome，得到 %s", welcome.Type)
	}
	var wp device.WelcomePayload
	if dErr := welcome.Decode(&wp); dErr != nil {
		t.Fatal(dErr)
	}
	if wp.DeviceID != paired.DeviceID || wp.HeartbeatS != 15 || wp.OfflineAfterS != 45 ||
		wp.ProtocolVersion != device.CurrentProtocolVersion {
		t.Fatalf("welcome 负载错误: %+v", wp)
	}

	// ⑤ ping → pong，并触发心跳落库
	ping := device.MustEnvelope(device.MsgPing, "p-1", device.PingPayload{
		Load:             json.RawMessage(`{"cpu":11}`),
		CapabilitiesHash: "live-hash",
		ProtocolVersion:  device.CurrentProtocolVersion,
	})
	rawPing, _ := json.Marshal(ping)
	if wErr := ws.Write(ctx, websocket.MessageText, rawPing); wErr != nil {
		t.Fatal(wErr)
	}
	_, data2, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("读取 pong 失败: %v", err)
	}
	var pong device.Envelope
	_ = json.Unmarshal(data2, &pong)
	if pong.Type != device.MsgPong || pong.ID != "p-1" {
		t.Fatalf("应为 pong(id=p-1)，得到 %s(id=%s)", pong.Type, pong.ID)
	}

	// ⑥ 列表应显示 online + 心跳写回的 capabilities_hash
	var list struct {
		Items []struct {
			ID               string `json:"id"`
			Status           string `json:"status"`
			CapabilitiesHash string `json:"capabilities_hash"`
			LastSeenAt       *int64 `json:"last_seen_at"`
		} `json:"items"`
	}
	waitUntil(t, 2*time.Second, func() bool {
		list.Items = nil
		doJSON(t, http.MethodGet, srv.URL+"/api/v1/devices", token, nil, http.StatusOK, &list)
		return len(list.Items) == 1 && list.Items[0].Status == "online" &&
			list.Items[0].CapabilitiesHash == "live-hash"
	})
	if list.Items[0].LastSeenAt == nil {
		t.Fatal("心跳应刷新 last_seen_at")
	}

	// ⑦ 吊销 → 服务端先发 device.revoked 错误帧，随后关闭连接
	doJSON(t, http.MethodDelete, srv.URL+"/api/v1/devices/"+paired.DeviceID, token, nil,
		http.StatusNoContent, nil)
	sawRevoked := false
	for i := 0; i < 3; i++ {
		_, revRaw, rErr := ws.Read(ctx)
		if rErr != nil {
			break // 连接已关闭
		}
		var revEnv device.Envelope
		if jErr := json.Unmarshal(revRaw, &revEnv); jErr == nil && revEnv.Type == device.MsgError {
			var ep device.ErrorPayload
			if dErr := revEnv.Decode(&ep); dErr == nil && ep.Code == device.FrameCodeRevoked {
				sawRevoked = true
			}
		}
	}
	if !sawRevoked {
		t.Fatal("吊销后应收到 device.revoked 错误帧")
	}
	// 连接最终必须关闭（读失败）
	if _, _, rErr := ws.Read(ctx); rErr == nil {
		t.Fatal("吊销后长连接应被服务端关闭")
	}
}

// TestAccessLogRedactsDeviceToken 铁律：WS 的 token query 永不落日志。
//
// 直接验证脱敏函数对该 query 的输出为空（白名单不含 token / device_token）。
func TestAccessLogRedactsDeviceToken(t *testing.T) {
	for _, q := range []string{
		"token=hgd_0123456789abcdef",
		"device_token=hgd_0123456789abcdef",
		"token=hgd_abc&protocol_version=1",
	} {
		got := observ.RedactQuery(q)
		if strings.Contains(got, "hgd_") {
			t.Fatalf("令牌泄露到日志: query=%q → %q", q, got)
		}
		if strings.Contains(got, "token") {
			t.Fatalf("连 token 键名都不应出现: query=%q → %q", q, got)
		}
	}
}

// waitUntil 轮询直到条件成立或超时。
func waitUntil(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}
