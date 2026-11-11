package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"harness-go/internal/device"
)

// sseFrame 解析出的单帧（event + data）。
type sseFrame struct {
	Event string
	Data  string
}

// readFrames 从 SSE 响应流里读 n 帧（跳过 `: ping` 注释行）。
//
// 用 bufio.Scanner 而不是一次 ReadAll：SSE 是**无限流**，ReadAll 会永远阻塞。
func readFrames(t *testing.T, r *bufio.Reader, n int, timeout time.Duration) []sseFrame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	out := make([]sseFrame, 0, n)
	var cur sseFrame
	for len(out) < n {
		if time.Now().After(deadline) {
			t.Fatalf("等待第 %d 帧超时（已收到 %d 帧：%+v）", n, len(out), out)
		}
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("读取 SSE 流失败：%v（已收到 %+v）", err, out)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			// 空行 = 一帧结束（仅当已收集到 data 时才算）。
			if cur.Data != "" {
				out = append(out, cur)
				cur = sseFrame{}
			}
		case strings.HasPrefix(line, ":"):
			// 注释行（`: ping` 保活），忽略。
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		}
	}
	return out
}

// newDeviceEventsServer 起一个只挂 deviceEvents 的测试服务器。
//
// 刻意不走完整 router：本测试关心的是 SSE 帧语义，而不是鉴权中间件
// （鉴权由 router_test / integration_test 覆盖）。
func newDeviceEventsServer(t *testing.T, bus *device.StateBus) *httptest.Server {
	t.Helper()
	h := &handlers{d: Deps{DeviceState: bus}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/devices/events", h.deviceEvents)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// openSSE 建立 SSE 连接并返回帧读取器与取消函数。
func openSSE(t *testing.T, url string) (*bufio.Reader, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatalf("构造请求失败：%v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("建立 SSE 连接失败：%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("SSE 应返回 200，得到 %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		cancel()
		t.Fatalf("Content-Type 应为 text/event-stream，得到 %q", ct)
	}
	return bufio.NewReader(resp.Body), cancel
}

func TestDeviceEventsDisabledWhenNoBus(t *testing.T) {
	h := &handlers{d: Deps{DeviceState: nil}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/devices/events", h.deviceEvents)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/devices/events")
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer resp.Body.Close()

	// 未装配设备域 → 503 device.disabled（而非挂起一个永不来的流）。
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("应返回 503，得到 %d", resp.StatusCode)
	}
}

func TestDeviceEventsFirstFrameIsSnapshot(t *testing.T) {
	bus := device.NewStateBus(device.StateBusOptions{})
	// 预置两台设备：首帧快照必须包含它们。
	bus.MarkOnline("dev-2", device.DeviceState{Name: "平板", Kind: "tablet"})
	bus.MarkOnline("dev-1", device.DeviceState{Name: "手机", Kind: "mobile"})

	srv := newDeviceEventsServer(t, bus)
	reader, cancel := openSSE(t, srv.URL+"/api/v1/devices/events")
	defer cancel()

	frames := readFrames(t, reader, 1, 3*time.Second)
	if frames[0].Event != device.StateEventSnapshot {
		t.Fatalf("首帧事件名应为 snapshot，得到 %q", frames[0].Event)
	}

	var payload device.DeviceStateEvent
	if err := json.Unmarshal([]byte(frames[0].Data), &payload); err != nil {
		t.Fatalf("快照 payload 解析失败：%v（原文 %q）", err, frames[0].Data)
	}
	if payload.Type != device.StateEventSnapshot {
		t.Fatalf("payload.type 应为 snapshot，得到 %q", payload.Type)
	}
	if len(payload.All) != 2 {
		t.Fatalf("快照应含 2 台设备，得到 %d", len(payload.All))
	}
	// 稳定排序：dev-1 在 dev-2 前。
	if payload.All[0].DeviceID != "dev-1" || payload.All[1].DeviceID != "dev-2" {
		t.Fatalf("快照应按 device_id 排序，得到 %+v", payload.All)
	}
	// 红线：快照里绝不能出现任何令牌字段。
	if strings.Contains(frames[0].Data, "token") {
		t.Fatalf("快照不得包含任何 token 字段：%q", frames[0].Data)
	}
}

func TestDeviceEventsReceivesOnlineAndOffline(t *testing.T) {
	bus := device.NewStateBus(device.StateBusOptions{})
	srv := newDeviceEventsServer(t, bus)
	reader, cancel := openSSE(t, srv.URL+"/api/v1/devices/events")
	defer cancel()

	// 消费首帧快照（此时为空）。
	first := readFrames(t, reader, 1, 3*time.Second)
	var snap device.DeviceStateEvent
	if err := json.Unmarshal([]byte(first[0].Data), &snap); err != nil {
		t.Fatalf("快照解析失败：%v", err)
	}
	if len(snap.All) != 0 {
		t.Fatalf("初始快照应为空，得到 %d 条", len(snap.All))
	}

	// 设备上线 → 必须收到 device.online。
	bus.MarkOnline("dev-1", device.DeviceState{Name: "手机", Kind: "mobile"})
	frames := readFrames(t, reader, 1, 3*time.Second)
	if frames[0].Event != device.StateEventDeviceOnline {
		t.Fatalf("应收到 device.online，得到 %q", frames[0].Event)
	}
	var online device.DeviceStateEvent
	if err := json.Unmarshal([]byte(frames[0].Data), &online); err != nil {
		t.Fatalf("online payload 解析失败：%v", err)
	}
	if online.Device == nil || online.Device.DeviceID != "dev-1" || !online.Device.Online {
		t.Fatalf("online 事件内容不符：%+v", online.Device)
	}
	if online.Device.Name != "手机" {
		t.Fatalf("应携带设备名，得到 %q", online.Device.Name)
	}

	// 设备离线 → 必须收到 device.offline。
	bus.MarkOffline("dev-1")
	frames = readFrames(t, reader, 1, 3*time.Second)
	if frames[0].Event != device.StateEventDeviceOffline {
		t.Fatalf("应收到 device.offline，得到 %q", frames[0].Event)
	}
	var offline device.DeviceStateEvent
	if err := json.Unmarshal([]byte(frames[0].Data), &offline); err != nil {
		t.Fatalf("offline payload 解析失败：%v", err)
	}
	if offline.Device == nil || offline.Device.Online {
		t.Fatalf("offline 事件内容不符：%+v", offline.Device)
	}
}

func TestDeviceEventsMultipleSubscribers(t *testing.T) {
	bus := device.NewStateBus(device.StateBusOptions{})
	srv := newDeviceEventsServer(t, bus)

	r1, c1 := openSSE(t, srv.URL+"/api/v1/devices/events")
	defer c1()
	r2, c2 := openSSE(t, srv.URL+"/api/v1/devices/events")
	defer c2()

	// 各自消费首帧快照。
	_ = readFrames(t, r1, 1, 3*time.Second)
	_ = readFrames(t, r2, 1, 3*time.Second)

	if n := bus.SubscriberCount(); n != 2 {
		t.Fatalf("应有 2 个订阅者，得到 %d", n)
	}

	// 一次上线，两个连接都必须收到。
	bus.MarkOnline("dev-1", device.DeviceState{Name: "手机"})
	f1 := readFrames(t, r1, 1, 3*time.Second)
	f2 := readFrames(t, r2, 1, 3*time.Second)
	if f1[0].Event != device.StateEventDeviceOnline {
		t.Fatalf("连接 1 未收到 online：%q", f1[0].Event)
	}
	if f2[0].Event != device.StateEventDeviceOnline {
		t.Fatalf("连接 2 未收到 online：%q", f2[0].Event)
	}
}

func TestDeviceEventsSubscribeBeforeSnapshotNoLostEvent(t *testing.T) {
	// 这条验证 handler 的关键顺序：**先 Subscribe 再 Snapshot**。
	// 若顺序反了，两次调用之间发生的上下线既不在快照也不在增量里 —— 会丢事件。
	bus := device.NewStateBus(device.StateBusOptions{})
	srv := newDeviceEventsServer(t, bus)
	reader, cancel := openSSE(t, srv.URL+"/api/v1/devices/events")
	defer cancel()

	_ = readFrames(t, reader, 1, 3*time.Second) // 首帧快照

	// 快照消费后立刻上线：必须通过增量送达（证明订阅在快照之前已建立）。
	bus.MarkOnline("late-dev", device.DeviceState{Name: "后到设备"})
	frames := readFrames(t, reader, 1, 3*time.Second)
	if frames[0].Event != device.StateEventDeviceOnline {
		t.Fatalf("应收到 late-dev 的 online 事件，得到 %q", frames[0].Event)
	}
	var ev device.DeviceStateEvent
	if err := json.Unmarshal([]byte(frames[0].Data), &ev); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if ev.Device == nil || ev.Device.DeviceID != "late-dev" {
		t.Fatalf("事件设备不符：%+v", ev.Device)
	}
}

func TestDeviceEventsUnsubscribesOnClientDisconnect(t *testing.T) {
	bus := device.NewStateBus(device.StateBusOptions{})
	srv := newDeviceEventsServer(t, bus)
	reader, cancel := openSSE(t, srv.URL+"/api/v1/devices/events")
	_ = readFrames(t, reader, 1, 3*time.Second)

	if n := bus.SubscriberCount(); n != 1 {
		t.Fatalf("应有 1 个订阅者，得到 %d", n)
	}

	// 客户端断开 → handler 必须退订（否则连接泄漏、总线订阅者无限增长）。
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if bus.SubscriberCount() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("客户端断开后应退订，仍有 %d 个订阅者", bus.SubscriberCount())
}
