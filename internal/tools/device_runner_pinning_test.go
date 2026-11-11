package tools

import (
	"context"
	"errors"
	"testing"

	"harness-go/internal/agent"
	"harness-go/internal/model"
	"harness-go/pkg/toolapi"
)

// ============ 执行面归属（D7 铁律）============
//
// 本文件锁死两件事，任意一条被回退都会让「切工作空间 = 在对应设备上操作」失效：
//   ① 指定了 DeviceID 时，DeviceTask.DeviceID 必须原样透传（否则派发器会挑别的设备）；
//   ② 指定设备不可用时**绝不静默降级**到服务端本地执行（必须明确报错）。
//
// 历史缺陷（勿回退）：tryDevice 从不填 DeviceTask.DeviceID，
// 导致 Dispatcher.candidates 走 hub.IDs() 分支——会话绑手机 A，写文件落到桌面端 B。

// recordingExecutor 记录被派发的任务，并可控返回结果/错误。
type recordingExecutor struct {
	tasks []DeviceTask
	res   DeviceTaskResult
	err   error
	// online 决定 Online() 返回值（0 = 无在线设备）。
	online int
}

func (r *recordingExecutor) Execute(_ context.Context, task DeviceTask) (DeviceTaskResult, error) {
	r.tasks = append(r.tasks, task)
	return r.res, r.err
}

func (r *recordingExecutor) Online() int { return r.online }

// deviceSpec 一个「可能在端侧执行」的工具契约。
func deviceSpec() toolapi.Spec {
	return toolapi.Spec{
		Name:     "write_file",
		Risk:     toolapi.RiskWrite,
		Executor: toolapi.ExecutorAny,
		Tier:     toolapi.TierCore,
	}
}

// ① 显式指定设备：DeviceTask.DeviceID 必须原样透传。
func TestTryDevicePropagatesExplicitDeviceID(t *testing.T) {
	exec := &recordingExecutor{
		online: 2, // 有别的在线设备，但我们必须只认指定的那台
		res:    DeviceTaskResult{Status: model.ToolOK, DeviceID: "dev-a", Result: []byte(`{"ok":1}`)},
	}
	r := &Runner{}
	r.SetDeviceExecutor(exec)

	_, ok := r.tryDevice(context.Background(), deviceSpec(), agent.ToolRunRequest{
		RunID: "run-1", ToolEventID: "ev-1", ToolName: "write_file", ArgsJSON: `{}`,
		DeviceID: "dev-a",
	})
	if !ok {
		t.Fatal("指定设备执行成功时应返回 ok=true")
	}
	if len(exec.tasks) != 1 {
		t.Fatalf("应派发 1 次，实际 %d 次", len(exec.tasks))
	}
	if got := exec.tasks[0].DeviceID; got != "dev-a" {
		t.Fatalf("DeviceTask.DeviceID 必须原样透传 dev-a（否则会派发到别的设备），得到 %q", got)
	}
}

// ② 指定设备不可用：必须明确失败（ok=true + Error），绝不降级本地。
func TestTryDeviceNoFallbackWhenDeviceUnavailable(t *testing.T) {
	exec := &recordingExecutor{
		err: ErrDeviceUnavailable,
		res: DeviceTaskResult{Degraded: true, Error: "device_offline"},
	}
	r := &Runner{}
	r.SetDeviceExecutor(exec)

	res, ok := r.tryDevice(context.Background(), deviceSpec(), agent.ToolRunRequest{
		RunID: "run-1", ToolEventID: "ev-1", ToolName: "write_file", ArgsJSON: `{}`,
		DeviceID: "dev-a",
	})
	if !ok {
		t.Fatal("指定设备不可用时**必须**返回 ok=true（ok=false 会被调用方降级到服务端本地执行）")
	}
	if res.Status != model.ToolError {
		t.Fatalf("应为 error 状态，得到 %q", res.Status)
	}
	if res.Error == "" {
		t.Fatal("必须带明确失败原因（用户与模型都要知道为什么不行）")
	}
	if res.Executor == model.ExecutorServer {
		t.Fatal("绝不能标记为服务端执行（那就是静默降级了）")
	}
}

// ③ 未指定设备（纯对话/内置空间）：保留旧的静默降级语义（ok=false）。
//
// 这条是「不破坏既有部署」的护栏：未接入端侧的环境本就应该本地执行。
func TestTryDeviceKeepsFallbackWhenNoDeviceSpecified(t *testing.T) {
	exec := &recordingExecutor{online: 0}
	r := &Runner{}
	r.SetDeviceExecutor(exec)

	_, ok := r.tryDevice(context.Background(), deviceSpec(), agent.ToolRunRequest{
		RunID: "run-1", ToolEventID: "ev-1", ToolName: "write_file", ArgsJSON: `{}`,
	})
	if ok {
		t.Fatal("无在线设备且未指定设备时应降级本地（ok=false）")
	}
	if len(exec.tasks) != 0 {
		t.Fatalf("无在线设备时不应发起派发，实际 %d 次", len(exec.tasks))
	}
}

// ④ 在途任务：指定设备路径同样不降级。
func TestTryDeviceInFlightOnExplicitDeviceDoesNotFallback(t *testing.T) {
	exec := &recordingExecutor{err: ErrDeviceTaskInFlight}
	r := &Runner{}
	r.SetDeviceExecutor(exec)

	res, ok := r.tryDevice(context.Background(), deviceSpec(), agent.ToolRunRequest{
		RunID: "run-1", ToolEventID: "ev-1", ToolName: "write_file", ArgsJSON: `{}`,
		DeviceID: "dev-a",
	})
	if !ok || res.Status != model.ToolError {
		t.Fatalf("在途应明确报错且不降级，得到 ok=%v status=%q", ok, res.Status)
	}
}

// ⑤ 非端侧工具（Executor=server）：无论是否指定设备都不派发。
func TestTryDeviceSkipsServerOnlyTool(t *testing.T) {
	exec := &recordingExecutor{online: 1}
	r := &Runner{}
	r.SetDeviceExecutor(exec)

	spec := toolapi.Spec{Name: "echo", Risk: toolapi.RiskReadonly, Executor: toolapi.ExecutorServer}
	_, ok := r.tryDevice(context.Background(), spec, agent.ToolRunRequest{
		RunID: "run-1", ToolEventID: "ev-1", ArgsJSON: `{}`, DeviceID: "dev-a",
	})
	if ok {
		t.Fatal("服务端工具不应派发到端侧")
	}
	if len(exec.tasks) != 0 {
		t.Fatalf("服务端工具不应产生派发，实际 %d 次", len(exec.tasks))
	}
}

// 确保 errors 包在断言辅助可用（部分子用例用到 errors.Is 语义扩展时使用）。
var _ = errors.Is
