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

	"harness-go/internal/model"
)

// ============ 任务派发（阶段 C-4）单测 ============
//
// 断言口径（对齐 多端协同 §9 验收矩阵）：
//   - task.dispatch / task.progress / task.result 三帧闭环，task_id 恒等于 tool_event_id
//   - 结果幂等：重复上报只收敛一次；未知 task_id 的迟到结果静默忽略
//   - deadline 到期 → timeout 失败（不悬空）
//   - 端侧不在线 / 断开 → **立即** Degraded（不等 TTL）
//   - 端侧结果恒 source_trust=untrusted

// dispatchHarness 派发测试脚手架（真实 WS 网关 + 假 Runner 客户端）。
type dispatchHarness struct {
	d   *Dispatcher
	hub *Hub
	srv *httptest.Server
}

func newDispatchHarness(t *testing.T, mutate func(*DispatcherOptions)) *dispatchHarness {
	t.Helper()
	hub := NewHub(HubOptions{OfflineAfter: 45 * time.Second})
	opts := DispatcherOptions{Hub: hub, TaskTTL: 2 * time.Second}
	if mutate != nil {
		mutate(&opts)
	}
	d := NewDispatcher(opts)
	gw := NewGateway(GatewayOptions{
		Hub: hub,
		Authenticate: func(context.Context, string) (*model.Device, error) {
			return &model.Device{ID: "dev-1", Name: "假 Runner", Status: model.DeviceOffline}, nil
		},
		Heartbeat:    15 * time.Second,
		OfflineAfter: 45 * time.Second,
		Handler:      d.HandleFrame,
		PendingCount: d.PendingCount,
	})
	srv := httptest.NewServer(http.HandlerFunc(gw.ServeHTTP))
	t.Cleanup(func() {
		srv.Close()
		hub.CloseAll("测试结束")
	})
	return &dispatchHarness{d: d, hub: hub, srv: srv}
}

// connect 以假 Runner 身份拨号并吃掉 welcome 帧。
func (h *dispatchHarness) connect(t *testing.T) (*websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	ws, _, err := websocket.Dial(ctx, wsURL(h.srv.URL, "hgd_good"), nil)
	if err != nil {
		t.Fatalf("假 Runner 拨号失败: %v", err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	env := readEnvelope(t, ctx, ws)
	if env.Type != MsgWelcome {
		t.Fatalf("首帧应为 welcome，得到 %s", env.Type)
	}
	waitFor(t, func() bool { return h.hub.Online("dev-1") })
	return ws, ctx
}

// readEnvelope 读取一帧并解码。
func readEnvelope(t *testing.T, ctx context.Context, ws *websocket.Conn) Envelope {
	t.Helper()
	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("读取帧失败: %v", err)
	}
	var env Envelope
	if uErr := json.Unmarshal(data, &env); uErr != nil {
		t.Fatalf("帧非法 JSON: %v", uErr)
	}
	return env
}

// sendFrame 发送一帧。
func sendFrame(t *testing.T, ctx context.Context, ws *websocket.Conn, env Envelope) {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if wErr := ws.Write(ctx, websocket.MessageText, raw); wErr != nil {
		t.Fatalf("写帧失败: %v", wErr)
	}
}

// awaitDispatch 等待并返回 task.dispatch 帧（跳过其它帧）。
func awaitDispatch(t *testing.T, ctx context.Context, ws *websocket.Conn) (Envelope, TaskDispatchPayload) {
	t.Helper()
	for i := 0; i < 6; i++ {
		env := readEnvelope(t, ctx, ws)
		if env.Type != MsgTaskDispatch {
			continue
		}
		var p TaskDispatchPayload
		if dErr := env.Decode(&p); dErr != nil {
			t.Fatalf("task.dispatch 负载解码失败: %v", dErr)
		}
		return env, p
	}
	t.Fatal("未收到 task.dispatch 帧")
	return Envelope{}, TaskDispatchPayload{}
}

// TestDispatcherDispatchSuccessAndTaskIDIsToolEventID 完整闭环 + 幂等键断言。
func TestDispatcherDispatchSuccessAndTaskIDIsToolEventID(t *testing.T) {
	h := newDispatchHarness(t, nil)
	ws, ctx := h.connect(t)

	done := make(chan TaskResult, 1)
	go func() {
		res, err := h.d.Dispatch(ctx, TaskRequest{
			RunID:       "run-1",
			ToolEventID: "evt-42",
			ToolName:    "fs.read",
			Args:        json.RawMessage(`{"path":"a.txt"}`),
		})
		if err != nil {
			t.Errorf("派发应成功，得到错误: %v", err)
		}
		done <- res
	}()

	env, p := awaitDispatch(t, ctx, ws)
	// ① task_id 恒等于 tool_event_id（端侧据此幂等）
	if p.TaskID != "evt-42" || p.ToolEventID != "evt-42" {
		t.Fatalf("task_id 必须等于 tool_event_id，得到 task_id=%q tool_event_id=%q", p.TaskID, p.ToolEventID)
	}
	if env.ID != "evt-42" || env.RunID != "run-1" || env.V != CurrentProtocolVersion {
		t.Fatalf("派发信封字段错误: %+v", env)
	}
	if p.ToolName != "fs.read" || string(p.Args) != `{"path":"a.txt"}` {
		t.Fatalf("派发负载错误: %+v", p)
	}
	if p.Deadline <= time.Now().UnixMilli() {
		t.Fatalf("deadline 必须是未来时间戳，得到 %d", p.Deadline)
	}
	if p.Attempt != 1 {
		t.Fatalf("首次派发 attempt 应为 1，得到 %d", p.Attempt)
	}

	// ② 回报进度（端侧只产事件，服务端不落库）
	progressEnv, _ := newEnvelopeAt(MsgTaskProgress, p.TaskID, TaskProgressPayload{
		TaskID: p.TaskID, RunID: p.RunID, ToolEventID: p.ToolEventID, Chunk: "读取中 50%",
	}, time.Now())
	progressEnv.RunID = p.RunID
	sendFrame(t, ctx, ws, progressEnv)

	// ③ 回报终态（端侧谎称 trusted 必须被强制覆盖为 untrusted）
	resultEnv := MustEnvelope(MsgTaskResult, p.TaskID, TaskResultPayload{
		TaskID:      p.TaskID,
		RunID:       p.RunID,
		ToolEventID: p.ToolEventID,
		Status:      TaskStatusSuccess,
		Result:      json.RawMessage(`{"content":"hello"}`),
		SourceTrust: string(model.TrustTrusted),
	})
	sendFrame(t, ctx, ws, resultEnv)

	res := <-done
	if res.Status != model.ToolOK {
		t.Fatalf("应为成功，得到 %s（%s）", res.Status, res.Error)
	}
	if string(res.Result) != `{"content":"hello"}` {
		t.Fatalf("结果未透传: %s", res.Result)
	}
	if res.SourceTrust != model.TrustUntrusted {
		t.Fatalf("端侧结果必须 untrusted，得到 %s", res.SourceTrust)
	}
	if res.DeviceID != "dev-1" || res.Degraded || res.Timeout {
		t.Fatalf("收敛结果字段错误: %+v", res)
	}
	if h.d.PendingCount("") != 0 {
		t.Fatalf("收敛后不应有在途任务，得到 %d", h.d.PendingCount(""))
	}
}

// TestDispatcherProgressAndIdempotentReplay 进度回调 + 重复结果幂等。
func TestDispatcherProgressAndIdempotentReplay(t *testing.T) {
	var mu sync.Mutex
	chunks := make([]string, 0, 4)
	h := newDispatchHarness(t, func(o *DispatcherOptions) {
		o.OnProgress = func(_ context.Context, deviceID string, p TaskProgressPayload) {
			if deviceID != "dev-1" {
				t.Errorf("进度回调设备应为 dev-1，得到 %q", deviceID)
			}
			mu.Lock()
			chunks = append(chunks, p.Chunk)
			mu.Unlock()
		}
	})
	ws, ctx := h.connect(t)

	done := make(chan TaskResult, 1)
	go func() {
		res, err := h.d.Dispatch(ctx, TaskRequest{
			ToolEventID: "evt-7", RunID: "run-7", ToolName: "grep", Args: json.RawMessage(`{}`),
		})
		if err != nil {
			t.Errorf("派发应成功: %v", err)
		}
		done <- res
	}()

	_, p := awaitDispatch(t, ctx, ws)
	for _, c := range []string{"a", "b", "c"} {
		env, _ := newEnvelopeAt(MsgTaskProgress, p.TaskID,
			TaskProgressPayload{TaskID: p.TaskID, Chunk: c}, time.Now())
		sendFrame(t, ctx, ws, env)
	}
	// 让进度帧先被处理（同连接串行读，顺序天然保证）
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(chunks) == 3
	})

	resEnv, _ := newEnvelopeAt(MsgTaskResult, p.TaskID, TaskResultPayload{
		TaskID: p.TaskID, Status: TaskStatusSuccess, Result: json.RawMessage(`{"ok":true}`),
	}, time.Now())
	// 幂等：同一终态连发两次，只应收敛一次且不 panic
	sendFrame(t, ctx, ws, resEnv)
	sendFrame(t, ctx, ws, resEnv)

	res := <-done
	if res.Status != model.ToolOK {
		t.Fatalf("应为成功，得到 %+v", res)
	}
	mu.Lock()
	got := append([]string(nil), chunks...)
	mu.Unlock()
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("进度顺序错误: %v", got)
	}

	// 幂等缓存：同一 task_id 再派发直接复用终态，不再下发新帧
	again, err := h.d.Dispatch(ctx, TaskRequest{
		ToolEventID: "evt-7", RunID: "run-7", ToolName: "grep", Args: json.RawMessage(`{}`),
	})
	if err != nil || again.Status != model.ToolOK {
		t.Fatalf("重复派发应命中幂等缓存: %+v err=%v", again, err)
	}
}

// TestDispatcherUnknownResultIgnored 未知 task_id 的迟到结果静默忽略（不断连）。
func TestDispatcherUnknownResultIgnored(t *testing.T) {
	h := newDispatchHarness(t, nil)
	ws, ctx := h.connect(t)

	env, _ := newEnvelopeAt(MsgTaskResult, "evt-ghost", TaskResultPayload{
		TaskID: "evt-ghost", Status: TaskStatusSuccess,
	}, time.Now())
	sendFrame(t, ctx, ws, env)

	// 连接仍可用：后续派发能正常闭环
	done := make(chan TaskResult, 1)
	go func() {
		res, _ := h.d.Dispatch(ctx, TaskRequest{ToolEventID: "evt-live", ToolName: "t"})
		done <- res
	}()
	_, p := awaitDispatch(t, ctx, ws)
	resEnv, _ := newEnvelopeAt(MsgTaskResult, p.TaskID, TaskResultPayload{
		TaskID: p.TaskID, Status: TaskStatusSuccess,
	}, time.Now())
	sendFrame(t, ctx, ws, resEnv)
	if res := <-done; res.Status != model.ToolOK {
		t.Fatalf("迟到结果不应破坏连接，得到 %+v", res)
	}
}

// TestDispatcherDeadlineTimeout deadline 到期必须收敛为 timeout（不悬空）。
func TestDispatcherDeadlineTimeout(t *testing.T) {
	h := newDispatchHarness(t, nil)
	_, ctx := h.connect(t) // 假 Runner 不回任何结果

	start := time.Now()
	res, err := h.d.Dispatch(ctx, TaskRequest{
		ToolEventID: "evt-timeout", ToolName: "slow", Timeout: 150 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("超时不应返回 error（走 Status 表达）: %v", err)
	}
	if res.Status != model.ToolTimeout || !res.Timeout || res.Error != TaskErrorTimeout {
		t.Fatalf("应判定 device_timeout，得到 %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("超时收敛过慢: %v", elapsed)
	}
	if res.DeviceID != "dev-1" || res.SourceTrust != model.TrustUntrusted {
		t.Fatalf("超时结果字段错误: %+v", res)
	}
}

// TestDispatcherOfflineImmediateDegrade 无在线设备立即降级（不变量 5）。
func TestDispatcherOfflineImmediateDegrade(t *testing.T) {
	h := newDispatchHarness(t, nil) // 不连接任何假 Runner

	start := time.Now()
	res, err := h.d.Dispatch(context.Background(), TaskRequest{
		ToolEventID: "evt-offline", ToolName: "fs.write",
	})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrDeviceUnavailable) {
		t.Fatalf("应返回 ErrDeviceUnavailable，得到 %v", err)
	}
	if !res.Degraded || res.Error != TaskErrorUnavailable {
		t.Fatalf("应立即降级，得到 %+v", res)
	}
	// 立即降级铁律：绝不能等到 deadline（默认 600s / 测试 2s）
	if elapsed > 300*time.Millisecond {
		t.Fatalf("离线降级必须立即返回，实际耗时 %v", elapsed)
	}
}

// TestDispatcherFailDeviceOnDisconnect 设备断开 → 在途任务立即收敛（不等 TTL）。
func TestDispatcherFailDeviceOnDisconnect(t *testing.T) {
	h := newDispatchHarness(t, nil)
	ws, ctx := h.connect(t)

	done := make(chan TaskResult, 1)
	go func() {
		res, _ := h.d.Dispatch(ctx, TaskRequest{ToolEventID: "evt-drop", ToolName: "t"})
		done <- res
	}()
	_, p := awaitDispatch(t, ctx, ws)

	// 模拟拔网：关闭连接 → hub 判离线 → 派发器立即降级
	ws.CloseNow()
	waitFor(t, func() bool { return !h.hub.Online("dev-1") })
	if n := h.d.FailDevice("dev-1", TaskErrorOffline); n != 1 {
		t.Fatalf("应有 1 个在途任务被降级，得到 %d", n)
	}

	select {
	case res := <-done:
		if !res.Degraded || res.Error != TaskErrorOffline || res.Status != model.ToolError {
			t.Fatalf("断连应立即降级为 device_offline，得到 %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("设备断开后任务未立即收敛（不应等 deadline）")
	}
	if p.TaskID != "evt-drop" {
		t.Fatalf("派发负载 task_id 错误: %q", p.TaskID)
	}
}

// TestDispatcherRejectsBadInput 参数校验与在途冲突。
func TestDispatcherRejectsBadInput(t *testing.T) {
	h := newDispatchHarness(t, nil)

	if _, err := h.d.Dispatch(context.Background(), TaskRequest{ToolName: "x"}); err == nil {
		t.Fatal("缺 task_id 应报错")
	}
	if _, err := h.d.Dispatch(context.Background(), TaskRequest{
		ToolEventID: "evt-bad", Args: json.RawMessage(`{bad`),
	}); err == nil {
		t.Fatal("非法 args 应报错")
	}
}

// TestDispatcherCancelPropagates 上层 ctx 取消不判端侧失败（交执行面按 cancelled 处理）。
func TestDispatcherCancelPropagates(t *testing.T) {
	h := newDispatchHarness(t, nil)
	_, wsCtx := h.connect(t)

	ctx, cancel := context.WithCancel(wsCtx)
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	_, err := h.d.Dispatch(ctx, TaskRequest{ToolEventID: "evt-cancel", ToolName: "t"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx 取消应返回 context.Canceled，得到 %v", err)
	}
}

// TestDispatcherCloseConcludesPending 服务端关闭时在途任务全部收敛。
func TestDispatcherCloseConcludesPending(t *testing.T) {
	h := newDispatchHarness(t, nil)
	ws, ctx := h.connect(t)

	done := make(chan TaskResult, 1)
	go func() {
		res, _ := h.d.Dispatch(ctx, TaskRequest{ToolEventID: "evt-close", ToolName: "t"})
		done <- res
	}()
	awaitDispatch(t, ctx, ws)

	h.d.Close(FrameCodeServerClosing)
	select {
	case res := <-done:
		if !res.Degraded || res.Error != FrameCodeServerClosing {
			t.Fatalf("关闭应对在途任务收敛为降级，得到 %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close 后在途任务未收敛")
	}
	// 重复关闭幂等（不得 panic / 阻塞）
	h.d.Close("again")
}
