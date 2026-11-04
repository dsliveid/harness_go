package main

// ============ Tier-1 证据：task.dispatch 端到端断言（C-4 + C-5 全链路） ============
//
// 为什么放在 cmd/harness-go：只有装配点能同时看见 device 与 tools/agent 两侧
// （internal/tools **不 import** internal/device，internal/device **不 import** internal/tools），
// 与 device_executor.go 的职责边界一致，端到端断言必须落在同一处。
//
// 链路（与生产装配完全同构，仅把「端侧 Runner 进程」换成测试内的假 WS 客户端）：
//
//	agent.Engine ─tool call─▶ tools.Runner(执行面路由)
//	                              └─ deviceTaskExecutor（cmd 适配层）
//	                                   └─ device.Dispatcher ─WS─▶ 假 Runner
//	                                            ▲ task.result
//	                              ┌─────────────┘
//	   终态回写 tool_events（executor / device_id / source_trust）→ 断言落库不变量
//
// 覆盖门禁：
//   - 成功路径：task.dispatch → task.result(success) → 落库 executor=device / untrusted / status=ok
//   - 失败路径：端侧回报 failed → 落库 status=error，**不降级本地重跑**（避免二次副作用）
//   - 拒绝路径：无在线设备 → 静默降级本地执行 → 落库 executor=server / trusted / status=ok，
//     且派发器零调用（不变量 5：不让用户等待 TTL，不向用户报错）
//
// 测试基建一律复用项目既有范式：httptest 服务端 + coder/websocket 客户端 + presetProvider，
// 不引入任何第三方测试库。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"harness-go/internal/agent"
	"harness-go/internal/approval"
	"harness-go/internal/device"
	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/model"
	"harness-go/internal/prompt"
	"harness-go/internal/service"
	"harness-go/internal/snapshot"
	"harness-go/internal/store"
	"harness-go/internal/tokenizer"
	"harness-go/internal/tools"
	"harness-go/internal/tools/hostimpl"
	"harness-go/internal/tools/loader"
	"harness-go/internal/tools/registry"
	"harness-go/internal/tools/router"
	"harness-go/pkg/toolapi"
)

// devE2EEnv task.dispatch 端到端环境（真实派发器 + 真实网关 + 真实引擎/落库）。
type devE2EEnv struct {
	engine     *agent.Engine
	sessions   *store.SessionStore
	runner     *tools.Runner
	dispatcher *device.Dispatcher
	hub        *device.Hub
	srv        *httptest.Server
	sessionID  string
	runID      string
}

// devE2EToolName 端侧优先工具名（executor=any：端侧在线走端侧，离线静默降级本地）。
const devE2EToolName = "sys.info"

// newDevE2EEnv 装配全链路环境。
//
// localInvoke 为「降级到本地」时真正执行的实现（用于断言拒绝路径确实本地执行）。
func newDevE2EEnv(t *testing.T, rounds [][]llm.Chunk, localInvoke func() (string, error)) *devE2EEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	main, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = main.Close() })
	sessions, err := store.OpenSessionStore(store.SessionStoreOptions{DataDir: dir, MaxOpen: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessions.Close() })
	bus := events.NewBus(100, 32)
	t.Cleanup(bus.Close)

	toolReg := registry.New(registry.Options{CoreSoftLimit: 8, DeferredSoftLimit: 60})
	toolHost := hostimpl.New(hostimpl.Options{WorkDir: dir})
	snaps, err := snapshot.New(snapshot.Options{Main: main, DataDir: dir, WorkspaceRoot: dir})
	if err != nil {
		t.Fatalf("快照存储初始化失败: %v", err)
	}
	toolHost.SetSnapshots(snaps)

	// executor=any + readonly：端侧可执行，且无需审批（聚焦派发链路本身）
	if regErr := toolReg.RegisterTool(toolapi.ToolFunc{
		S: toolapi.Spec{
			Name:        devE2EToolName,
			Description: "端到端测试用端侧优先工具",
			SchemaJSON:  []byte(`{"type":"object"}`),
			Tier:        toolapi.TierCore,
			Executor:    toolapi.ExecutorAny,
			Risk:        toolapi.RiskReadonly,
		},
		InvokeF: func(context.Context, *toolapi.Call) (*toolapi.Result, error) {
			content, iErr := localInvoke()
			if iErr != nil {
				return nil, iErr
			}
			return &toolapi.Result{Content: content}, nil
		},
	}, "test"); regErr != nil {
		t.Fatalf("注册端侧工具失败: %v", regErr)
	}

	runner := tools.NewRunner(toolReg, router.New(router.Options{Executor: toolapi.ExecutorServer}),
		loader.Limits{CoreSoft: 8, DeferredSoft: 60})

	// ---- 设备域装配（与 cmd 生产装配同构）----
	hub := device.NewHub(device.HubOptions{OfflineAfter: 45 * time.Second})
	t.Cleanup(func() { hub.CloseAll("测试结束") })
	dispatcher := device.NewDispatcher(device.DispatcherOptions{Hub: hub, TaskTTL: 5 * time.Second})
	gw := device.NewGateway(device.GatewayOptions{
		Hub: hub,
		Authenticate: func(context.Context, string) (*model.Device, error) {
			return &model.Device{ID: "dev-1", Name: "假 Runner", Status: model.DeviceOffline}, nil
		},
		Heartbeat:    15 * time.Second,
		OfflineAfter: 45 * time.Second,
		Handler:      dispatcher.HandleFrame,
		PendingCount: dispatcher.PendingCount,
	})
	srv := httptest.NewServer(http.HandlerFunc(gw.ServeHTTP))
	t.Cleanup(srv.Close)

	// 适配层：device.Dispatcher → tools.DeviceExecutor（cmd/device_executor.go）
	exec := newDeviceTaskExecutor(hub, dispatcher, nil)
	// 计数包装：拒绝路径断言「端侧零调用」
	runner.SetDeviceExecutor(&countingDeviceExecutor{inner: exec, calls: &envDeviceCalls})

	provider := &presetProvider{rounds: rounds}
	gate := approval.NewGate(time.Second)
	engine, err := agent.New(agent.Deps{
		Sessions: service.NewSessionProvider(sessions),
		Summary:  service.NewSummaryWriter(main),
		Events:   bus,
		Models:   fsEnvResolver{provider},
		Builder:  prompt.NewBuilder(tokenizer.NewHeuristic(), prompt.DefaultLimits()),
		Tools:    runner,
		Catalog:  runner,
		Gate:     gate,
		MaxSteps: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessSvc := service.NewSessionService(main, sessions)
	sess, err := sessSvc.Create(ctx, "device-e2e", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return &devE2EEnv{
		engine: engine, sessions: sessions, runner: runner,
		dispatcher: dispatcher, hub: hub, srv: srv, sessionID: sess.ID,
	}
}

// envDeviceCalls 端侧执行面调用计数（测试内共享；每次装配重置）。
var envDeviceCalls int32

// countingDeviceExecutor 包装真实适配层，统计端侧调用次数。
type countingDeviceExecutor struct {
	inner tools.DeviceExecutor
	calls *int32
}

func (c *countingDeviceExecutor) Execute(ctx context.Context, task tools.DeviceTask) (tools.DeviceTaskResult, error) {
	atomic.AddInt32(c.calls, 1)
	return c.inner.Execute(ctx, task)
}

func (c *countingDeviceExecutor) Online() int { return c.inner.Online() }

// startRun 预建 Run 行并以 full 模式同步执行（端到端主流程）。
func (e *devE2EEnv) startRun(t *testing.T, runID, input string) {
	t.Helper()
	ctx := context.Background()
	sdb, err := e.sessions.Acquire(ctx, e.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sdb.Runs().Create(ctx, &model.Run{
		ID: runID, Status: model.RunPending, ModelRef: "test-model", Executor: model.ExecutorServer,
	}); err != nil {
		t.Fatal(err)
	}
	e.runID = runID
	if err := e.engine.Execute(ctx, agent.RunRequest{
		RunID: runID, SessionID: e.sessionID, UserInput: input,
		ModelRef: "test-model", AccessMode: approval.ModeFull,
		SystemBase: "测试",
	}); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
}

// toolEvents 读取本 Run 落库的工具事件。
func (e *devE2EEnv) toolEvents(t *testing.T) []model.ToolEvent {
	t.Helper()
	sdb, err := e.sessions.Acquire(context.Background(), e.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := sdb.ToolEvents().ListByRun(context.Background(), e.runID)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// dialRunner 以假 Runner 身份连上网关并吃掉 welcome 帧。
func dialRunner(t *testing.T, env *devE2EEnv) (*websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	href := "ws" + strings.TrimPrefix(env.srv.URL, "http") + "/?token=hgd_test"
	ws, _, err := websocket.Dial(ctx, href, nil)
	if err != nil {
		t.Fatalf("假 Runner 拨号失败: %v", err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	// 首帧 welcome
	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("读取 welcome 失败: %v", err)
	}
	var env0 device.Envelope
	if uErr := json.Unmarshal(data, &env0); uErr != nil {
		t.Fatalf("welcome 帧非法 JSON: %v", uErr)
	}
	if env0.Type != device.MsgWelcome {
		t.Fatalf("首帧应为 welcome，得到 %s", env0.Type)
	}
	// 等注册完成（Online 才为 true）
	deadline := time.Now().Add(3 * time.Second)
	for env.hub.Count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("假 Runner 未在超时内上线")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return ws, ctx
}

// readDispatch 等待 task.dispatch 帧（跳过其它帧）。
func readDispatch(t *testing.T, ctx context.Context, ws *websocket.Conn) (device.Envelope, device.TaskDispatchPayload) {
	t.Helper()
	for i := 0; i < 8; i++ {
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("读取帧失败: %v", err)
		}
		var env device.Envelope
		if uErr := json.Unmarshal(data, &env); uErr != nil {
			t.Fatalf("帧非法 JSON: %v", uErr)
		}
		if env.Type != device.MsgTaskDispatch {
			continue
		}
		var p device.TaskDispatchPayload
		if dErr := env.Decode(&p); dErr != nil {
			t.Fatalf("task.dispatch 负载解码失败: %v", dErr)
		}
		return env, p
	}
	t.Fatal("未收到 task.dispatch 帧")
	return device.Envelope{}, device.TaskDispatchPayload{}
}

// replyResult 以假 Runner 身份回报终态。
func replyResult(t *testing.T, ctx context.Context, ws *websocket.Conn, p device.TaskDispatchPayload,
	status string, result string, errText string) {
	t.Helper()
	payload := device.TaskResultPayload{
		TaskID:      p.TaskID,
		RunID:       p.RunID,
		ToolEventID: p.ToolEventID,
		Status:      status,
		Error:       errText,
	}
	if result != "" {
		payload.Result = json.RawMessage(result)
	}
	env, err := device.NewEnvelope(device.MsgTaskResult, p.TaskID, payload)
	if err != nil {
		t.Errorf("构造 task.result 帧失败: %v", err)
		return
	}
	env.RunID = p.RunID
	raw, mErr := json.Marshal(env)
	if mErr != nil {
		t.Errorf("序列化 task.result 失败: %v", mErr)
		return
	}
	if wErr := ws.Write(ctx, websocket.MessageText, raw); wErr != nil {
		t.Errorf("写 task.result 失败: %v", wErr)
	}
}

// deviceContent 解析 tools 层包装后的结果 JSON（{"content","meta"}），返回内容与 meta。
func deviceContent(t *testing.T, raw json.RawMessage) (string, map[string]any) {
	t.Helper()
	var parsed struct {
		Content string         `json:"content"`
		Meta    map[string]any `json:"meta"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("端侧结果必须是合法 {\"content\",\"meta\"} JSON: %v（%s）", err, raw)
	}
	return parsed.Content, parsed.Meta
}

// ---- 用例 1：派发成功路径 ----

// TestDeviceDispatchE2E_SuccessPersistsDeviceAttribution
// 不变量：
//  1. 端侧在线时工具调用**必须走端侧**（executor=device、device_id=dev-1）；
//  2. 端侧结果一律 source_trust=untrusted（不变量 4），落库亦然；
//  3. task_id 恒等于 tool_event_id（端侧幂等键）；
//  4. 端侧返回内容必须包装成 {"content","meta"} 合法 JSON 落库，Run 正常完成。
func TestDeviceDispatchE2E_SuccessPersistsDeviceAttribution(t *testing.T) {
	atomic.StoreInt32(&envDeviceCalls, 0)
	env := newDevE2EEnv(t, [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{ID: "call-1", Name: devE2EToolName,
			ArgumentsDelta: `{"q":"hi"}`}}},
		{{Delta: "端侧已处理"}, {Usage: &llm.Usage{TotalTokens: 7}}},
	}, func() (string, error) {
		t.Error("端侧在线时不得在本地执行")
		return "", nil
	})

	ws, ctx := dialRunner(t, env)

	// 假 Runner 协程：收派发 → 校验 → 回终态
	dispatchSeen := make(chan device.TaskDispatchPayload, 1)
	go func() {
		_, p := readDispatch(t, ctx, ws)
		dispatchSeen <- p
		replyResult(t, ctx, ws, p, device.TaskStatusSuccess, `{"stdout":"ok-from-device"}`, "")
	}()

	env.startRun(t, "run-dev-ok", "看下系统信息")

	p := <-dispatchSeen
	// ③ 幂等键不变量：task_id 恒等于 tool_event_id，且来自引擎生成的工具事件 ID
	if p.TaskID == "" || p.TaskID != p.ToolEventID {
		t.Fatalf("task_id 必须等于 tool_event_id，得到 task_id=%q tool_event_id=%q", p.TaskID, p.ToolEventID)
	}
	if p.RunID != "run-dev-ok" || p.ToolName != devE2EToolName {
		t.Fatalf("派发负载路由字段错误: %+v", p)
	}
	if string(p.Args) != `{"q":"hi"}` {
		t.Fatalf("派发参数必须原样透传，得到 %s", p.Args)
	}

	evs := env.toolEvents(t)
	if len(evs) != 1 {
		t.Fatalf("应有 1 个工具事件，得到 %d", len(evs))
	}
	ev := evs[0]
	if ev.ID != p.ToolEventID {
		t.Fatalf("落库事件 ID 应等于派发 task_id: %s != %s", ev.ID, p.ToolEventID)
	}
	if ev.Status != model.ToolOK {
		t.Fatalf("工具事件应为 ok，得到 %s（error=%s）", ev.Status, ev.Error)
	}
	if ev.Executor != model.ExecutorDevice {
		t.Fatalf("执行面应归属 device，得到 %s（端侧在线却落成本地=执行面路由缺陷）", ev.Executor)
	}
	if ev.DeviceID != "dev-1" {
		t.Fatalf("device_id 应为 dev-1，得到 %q", ev.DeviceID)
	}
	if ev.SourceTrust != model.TrustUntrusted {
		t.Fatalf("端侧结果必须 untrusted（不变量 4），得到 %s", ev.SourceTrust)
	}
	content, meta := deviceContent(t, ev.ResultJSON)
	if !strings.Contains(content, "ok-from-device") {
		t.Fatalf("端侧返回内容未落库，得到 %q", content)
	}
	if meta["executor"] != "device" || meta["source_trust"] != "untrusted" || meta["device_id"] != "dev-1" {
		t.Fatalf("结果 meta 执行面归属错误: %+v", meta)
	}
	if got := atomic.LoadInt32(&envDeviceCalls); got != 1 {
		t.Fatalf("端侧执行面应恰好调用 1 次，实际 %d", got)
	}
	if env.dispatcher.PendingCount("") != 0 {
		t.Fatalf("终态收敛后不应有在途任务，得到 %d", env.dispatcher.PendingCount(""))
	}

	// Run 必须正常完成（端侧成功不阻塞状态机）
	sdb, err := env.sessions.Acquire(context.Background(), env.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := sdb.Runs().Get(context.Background(), "run-dev-ok")
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || run.Status != model.RunCompleted {
		t.Fatalf("Run 应完成: %+v", run)
	}
}

// ---- 用例 2：失败路径 ----

// TestDeviceDispatchE2E_DeviceFailureNotRerunLocally
// 不变量：端侧**真实执行并失败**时，必须采用端侧终态（status=error + executor=device + untrusted），
// **禁止**降级本地重跑（否则产生二次副作用），且 Run 仍能走完（模型拿到错误继续）。
func TestDeviceDispatchE2E_DeviceFailureNotRerunLocally(t *testing.T) {
	atomic.StoreInt32(&envDeviceCalls, 0)
	env := newDevE2EEnv(t, [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{ID: "call-1", Name: devE2EToolName,
			ArgumentsDelta: `{"q":"hi"}`}}},
		{{Delta: "端侧失败了，我换个办法"}, {Usage: &llm.Usage{TotalTokens: 6}}},
	}, func() (string, error) {
		t.Error("端侧真实失败时不得降级本地重跑（二次副作用）")
		return "", nil
	})

	ws, ctx := dialRunner(t, env)
	go func() {
		_, p := readDispatch(t, ctx, ws)
		replyResult(t, ctx, ws, p, device.TaskStatusFailed, "", "permission denied: /root")
	}()

	env.startRun(t, "run-dev-fail", "看下系统信息")

	evs := env.toolEvents(t)
	if len(evs) != 1 {
		t.Fatalf("应有 1 个工具事件，得到 %d", len(evs))
	}
	ev := evs[0]
	if ev.Status != model.ToolError {
		t.Fatalf("端侧 failed 应落库 error，得到 %s", ev.Status)
	}
	if ev.Executor != model.ExecutorDevice || ev.DeviceID != "dev-1" {
		t.Fatalf("失败仍归属端侧: executor=%s device=%q", ev.Executor, ev.DeviceID)
	}
	if ev.SourceTrust != model.TrustUntrusted {
		t.Fatalf("端侧（含失败）结果必须 untrusted，得到 %s", ev.SourceTrust)
	}
	if !strings.Contains(ev.Error, "permission denied") {
		t.Fatalf("端侧错误信息应透传落库，得到 %q", ev.Error)
	}
	if got := atomic.LoadInt32(&envDeviceCalls); got != 1 {
		t.Fatalf("失败路径端侧应只调用 1 次（不得本地重跑），实际 %d", got)
	}

	// 失败原因是工具级失败，不是 Run 失败：状态机必须走完
	sdb, err := env.sessions.Acquire(context.Background(), env.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := sdb.Runs().Get(context.Background(), "run-dev-fail")
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || run.Status != model.RunCompleted {
		t.Fatalf("工具失败不应中断 Run: %+v", run)
	}
}

// ---- 用例 3：拒绝/降级路径 ----

// TestDeviceDispatchE2E_NoDeviceSilentlyDegradesToLocal
// 不变量（不变量 5）：无在线设备时必须**静默降级本地执行**——
//   - 派发器零点调用（不让用户等 TTL）；
//   - 落库 executor=server / source_trust=trusted / status=ok；
//   - 用户侧无任何错误（工具事件 Error 为空，Run 完成）。
func TestDeviceDispatchE2E_NoDeviceSilentlyDegradesToLocal(t *testing.T) {
	atomic.StoreInt32(&envDeviceCalls, 0)
	var localCalls int32
	env := newDevE2EEnv(t, [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{ID: "call-1", Name: devE2EToolName,
			ArgumentsDelta: `{"q":"hi"}`}}},
		{{Delta: "已在本机执行"}, {Usage: &llm.Usage{TotalTokens: 5}}},
	}, func() (string, error) {
		atomic.AddInt32(&localCalls, 1)
		return "local-fallback-result", nil
	})
	// 注意：本用例**不拨号任何假 Runner**，Hub 中零在线设备。

	env.startRun(t, "run-degrade", "看下系统信息")

	if got := atomic.LoadInt32(&envDeviceCalls); got != 0 {
		t.Fatalf("无在线设备时不得发起端侧派发（不让用户等 TTL），实际调用 %d 次", got)
	}
	if got := atomic.LoadInt32(&localCalls); got != 1 {
		t.Fatalf("应静默降级到本地执行恰好 1 次，实际 %d", got)
	}

	evs := env.toolEvents(t)
	if len(evs) != 1 {
		t.Fatalf("应有 1 个工具事件，得到 %d", len(evs))
	}
	ev := evs[0]
	if ev.Status != model.ToolOK {
		t.Fatalf("降级本地后应成功，得到 %s（error=%s）", ev.Status, ev.Error)
	}
	if ev.Executor != model.ExecutorServer {
		t.Fatalf("降级后执行面必须落 server，得到 %s", ev.Executor)
	}
	if ev.DeviceID != "" {
		t.Fatalf("降级后不应有 device_id，得到 %q", ev.DeviceID)
	}
	if ev.SourceTrust != model.TrustTrusted {
		t.Fatalf("本地执行结果应为 trusted，得到 %s", ev.SourceTrust)
	}
	if ev.Error != "" {
		t.Fatalf("降级必须静默（不变量 5），工具事件不应带错误，得到 %q", ev.Error)
	}
	if !strings.Contains(string(ev.ResultJSON), "local-fallback-result") {
		t.Fatalf("本地执行结果未落库: %s", ev.ResultJSON)
	}

	sdb, err := env.sessions.Acquire(context.Background(), env.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := sdb.Runs().Get(context.Background(), "run-degrade")
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || run.Status != model.RunCompleted {
		t.Fatalf("降级路径 Run 应完成: %+v", run)
	}
}
