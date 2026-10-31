package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"harness-go/internal/agent"
	"harness-go/internal/model"
	"harness-go/internal/tools/loader"
	"harness-go/internal/tools/registry"
	"harness-go/internal/tools/router"
	"harness-go/pkg/toolapi"
)

// ============ C-5 执行面路由测试 ============
//
// 断言口径（对齐 多端协同 §9 验收矩阵）：
//   - device/any 且有在线设备 → 端侧执行，executor=device / source_trust=untrusted
//   - 无在线设备 / 派发降级 → **静默降级**本地，executor=server / trusted / status=ok（无用户可见错误）
//   - 端侧真实失败/超时 → 直接采用端侧终态，不重跑本地（避免二次副作用）
//   - spec.Executor==server 的工具**永不**触碰端侧
//   - 结果 JSON 必须合法（tool_events.result_json 是 json.RawMessage）

// fakeDevice 端侧执行面替身。
type fakeDevice struct {
	online int
	calls  int
	last   DeviceTask
	result DeviceTaskResult
	err    error
}

func (d *fakeDevice) Execute(_ context.Context, task DeviceTask) (DeviceTaskResult, error) {
	d.calls++
	d.last = task
	return d.result, d.err
}

func (d *fakeDevice) Online() int { return d.online }

// newC5Runner 构造带端侧执行面的 Runner（localCalls 统计本地执行次数）。
func newC5Runner(t *testing.T, dev DeviceExecutor, specs ...toolapi.Spec) (*Runner, *int) {
	t.Helper()
	reg := registry.New(registry.Options{})
	localCalls := 0
	for _, s := range specs {
		s := s
		tool := toolapi.ToolFunc{S: s, InvokeF: func(_ context.Context, call *toolapi.Call) (*toolapi.Result, error) {
			localCalls++
			return &toolapi.Result{Content: "local: " + string(call.Args)}, nil
		}}
		if err := reg.RegisterTool(tool, "test"); err != nil {
			t.Fatalf("注册测试工具失败: %v", err)
		}
	}
	rtr := router.New(router.Options{Executor: toolapi.ExecutorServer})
	r := NewRunner(reg, rtr, loader.Limits{CoreSoft: 8, DeferredSoft: 60})
	r.SetDeviceExecutor(dev)
	return r, &localCalls
}

func deviceSpecOf(name string, exec toolapi.Executor) toolapi.Spec {
	return toolapi.Spec{Name: name, Description: name + " 描述", Tier: toolapi.TierCore,
		Risk: toolapi.RiskReadonly, Executor: exec, SchemaJSON: []byte(`{"type":"object"}`)}
}

func c5Req(name string) agent.ToolRunRequest {
	return agent.ToolRunRequest{
		RunID: "run-1", SessionID: "s-1", ToolEventID: "ev-1",
		ToolName: name, ArgsJSON: `{"text":"hi"}`, AccessMode: "full",
	}
}

// TestDevicePreferred 端侧在线：device/any 工具走端侧，结果 untrusted。
func TestDevicePreferred(t *testing.T) {
	dev := &fakeDevice{
		online: 1,
		result: DeviceTaskResult{
			DeviceID: "dev-1", Status: model.ToolOK,
			Result:      json.RawMessage(`{"echo":"hi"}`),
			SourceTrust: model.TrustUntrusted,
		},
	}
	r, localCalls := newC5Runner(t, dev, deviceSpecOf("fs", toolapi.ExecutorAny))

	res, err := r.Run(context.Background(), c5Req("fs"))
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if *localCalls != 0 {
		t.Fatalf("应走端侧，本地不应被调用（%d 次）", *localCalls)
	}
	if dev.calls != 1 || dev.last.TaskID != "ev-1" {
		t.Fatalf("派发参数异常: calls=%d task=%+v", dev.calls, dev.last)
	}
	if res.Status != model.ToolOK || res.Executor != model.ExecutorDevice || res.DeviceID != "dev-1" {
		t.Fatalf("执行面归属错误: %+v", res)
	}
	if res.SourceTrust != model.TrustUntrusted {
		t.Fatalf("端侧结果必须 untrusted（不变量 4），得到 %q", res.SourceTrust)
	}
	if !json.Valid([]byte(res.ResultJSON)) {
		t.Fatalf("端侧结果包装后必须合法 JSON: %q", res.ResultJSON)
	}
	var payload struct {
		Content string `json:"content"`
		Meta    struct {
			Executor string `json:"executor"`
			DeviceID string `json:"device_id"`
		} `json:"meta"`
	}
	if uErr := json.Unmarshal([]byte(res.ResultJSON), &payload); uErr != nil {
		t.Fatalf("结果解析失败: %v", uErr)
	}
	if payload.Meta.Executor != "device" || payload.Meta.DeviceID != "dev-1" {
		t.Fatalf("结果 meta 缺执行面信息: %+v", payload.Meta)
	}
	if payload.Content != `{"echo":"hi"}` {
		t.Fatalf("端侧原始结果应作为 content 透出: %q", payload.Content)
	}
}

// TestSilentDegradeWhenNoDevice 无在线设备：不派发、立即本地执行、无用户可见错误。
func TestSilentDegradeWhenNoDevice(t *testing.T) {
	dev := &fakeDevice{online: 0}
	r, localCalls := newC5Runner(t, dev, deviceSpecOf("fs", toolapi.ExecutorAny))

	res, err := r.Run(context.Background(), c5Req("fs"))
	if err != nil {
		t.Fatalf("降级不应返回 error: %v", err)
	}
	if dev.calls != 0 {
		t.Fatalf("无在线设备时不应发起派发，实际 %d 次", dev.calls)
	}
	if *localCalls != 1 {
		t.Fatalf("应降级本地执行 1 次，实际 %d", *localCalls)
	}
	if res.Status != model.ToolOK || res.Executor != model.ExecutorServer ||
		res.SourceTrust != model.TrustTrusted || res.Error != "" {
		t.Fatalf("降级结果必须 ok + server + trusted + 无错误: %+v", res)
	}
}

// TestSilentDegradeWhenDispatchDegraded 派发器报 degraded（断连/写帧失败）→ 静默降级。
func TestSilentDegradeWhenDispatchDegraded(t *testing.T) {
	dev := &fakeDevice{
		online: 1,
		result: DeviceTaskResult{Degraded: true, Error: "device_offline", SourceTrust: model.TrustUntrusted},
	}
	r, localCalls := newC5Runner(t, dev, deviceSpecOf("fs", toolapi.ExecutorAny))

	res, _ := r.Run(context.Background(), c5Req("fs"))
	if *localCalls != 1 || res.Executor != model.ExecutorServer || res.Status != model.ToolOK {
		t.Fatalf("degraded 应静默降级本地: localCalls=%d res=%+v", *localCalls, res)
	}
}

// TestSilentDegradeOnDeviceError 端侧异常（协议/帧错误）→ 同样降级，不阻塞 Run。
func TestSilentDegradeOnDeviceError(t *testing.T) {
	for _, target := range []error{ErrDeviceUnavailable, errors.New("帧写出失败")} {
		dev := &fakeDevice{online: 1, err: target}
		r, localCalls := newC5Runner(t, dev, deviceSpecOf("fs", toolapi.ExecutorAny))
		res, _ := r.Run(context.Background(), c5Req("fs"))
		if *localCalls != 1 || res.Status != model.ToolOK || res.Executor != model.ExecutorServer {
			t.Fatalf("端侧错误 %v 应降级本地: localCalls=%d res=%+v", target, *localCalls, res)
		}
	}
}

// TestDeviceFailureNotRetriedLocally 端侧真实执行失败/超时 → 采用端侧终态，**不重跑本地**。
func TestDeviceFailureNotRetriedLocally(t *testing.T) {
	cases := []struct {
		name string
		res  DeviceTaskResult
		want model.ToolEventStatus
	}{
		{"failed", DeviceTaskResult{DeviceID: "dev-1", Status: model.ToolError, Error: "device_bad_result"}, model.ToolError},
		{"timeout", DeviceTaskResult{DeviceID: "dev-1", Status: model.ToolTimeout, Error: "device_timeout", Timeout: true}, model.ToolTimeout},
	}
	for _, c := range cases {
		dev := &fakeDevice{online: 1, result: c.res}
		r, localCalls := newC5Runner(t, dev, deviceSpecOf("fs", toolapi.ExecutorAny))
		res, _ := r.Run(context.Background(), c5Req("fs"))
		if *localCalls != 0 {
			t.Fatalf("%s：端侧已执行过，禁止本地重跑（会二次副作用），实际 %d 次", c.name, *localCalls)
		}
		if res.Status != c.want || res.Executor != model.ExecutorDevice ||
			res.SourceTrust != model.TrustUntrusted || res.Error == "" {
			t.Fatalf("%s：应采信端侧终态且 untrusted，得到 %+v", c.name, res)
		}
	}
}

// TestServerToolNeverTouchesDevice spec.Executor==server 的工具永不派发端侧。
func TestServerToolNeverTouchesDevice(t *testing.T) {
	dev := &fakeDevice{online: 1}
	r, localCalls := newC5Runner(t, dev, deviceSpecOf("local", toolapi.ExecutorServer))

	res, _ := r.Run(context.Background(), c5Req("local"))
	if dev.calls != 0 {
		t.Fatalf("server 工具不应派发端侧，实际 %d 次", dev.calls)
	}
	if *localCalls != 1 || res.Executor != model.ExecutorServer || res.SourceTrust != model.TrustTrusted {
		t.Fatalf("server 工具应本地执行且 trusted: localCalls=%d res=%+v", *localCalls, res)
	}
}

// TestNilDeviceExecutorKeepsLegacyBehavior 未注入端侧执行面时行为与 C 阶段前完全一致。
func TestNilDeviceExecutorKeepsLegacyBehavior(t *testing.T) {
	reg := registry.New(registry.Options{})
	if err := reg.RegisterTool(toolapi.ToolFunc{
		S: deviceSpecOf("any", toolapi.ExecutorAny),
		InvokeF: func(context.Context, *toolapi.Call) (*toolapi.Result, error) {
			return &toolapi.Result{Content: "ok"}, nil
		},
	}, "test"); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	r := NewRunner(reg, router.New(router.Options{Executor: toolapi.ExecutorServer}),
		loader.Limits{CoreSoft: 8, DeferredSoft: 60})
	if r.DeviceExecutor() != nil {
		t.Fatal("默认应无端侧执行面")
	}
	res, _ := r.Run(context.Background(), c5Req("any"))
	if res.Status != model.ToolOK || res.Executor != model.ExecutorServer || res.SourceTrust != model.TrustTrusted {
		t.Fatalf("未接设备域时应回退本地 server/trusted: %+v", res)
	}
}

// TestDegradeStillEnforcesLocalRouter 降级后仍须过本端能力校验（宽进严出不绕开）。
func TestDegradeStillEnforcesLocalRouter(t *testing.T) {
	dev := &fakeDevice{online: 0}
	spec := deviceSpecOf("needs-net", toolapi.ExecutorAny)
	spec.Capabilities = []string{toolapi.CapNetwork}
	reg := registry.New(registry.Options{})
	if err := reg.RegisterTool(toolapi.ToolFunc{
		S: spec,
		InvokeF: func(context.Context, *toolapi.Call) (*toolapi.Result, error) {
			t.Fatal("缺少能力时不应真正执行")
			return nil, nil
		},
	}, "test"); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	// 本端不具备 network 能力
	r := NewRunner(reg, router.New(router.Options{Executor: toolapi.ExecutorServer}),
		loader.Limits{CoreSoft: 8, DeferredSoft: 60})
	r.SetDeviceExecutor(dev)

	res, _ := r.Run(context.Background(), c5Req("needs-net"))
	if res.Status != model.ToolError || res.Error == "" {
		t.Fatalf("降级后缺少本端能力应明确报错: %+v", res)
	}
}
