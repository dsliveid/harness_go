package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"harness-go/internal/agent"
	"harness-go/internal/model"
	"harness-go/pkg/toolapi"
)

// ============ 执行面委派（阶段 C-5，多端协同 HALO） ============
//
// 职责边界（与 C-4 派发器严格分工，勿越界）：
//   - 本文件只做「**路由**」：能派端侧就派端侧，端侧不可用就**静默降级**本地执行
//   - 端侧不可用（无在线设备 / 断连 / 写帧失败）→ 立即降级本地，**绝不向用户报错**（不变量 5）
//   - 端侧真正执行过（含失败/超时）→ 结果按真实回报落地，**不重跑**（避免重复副作用）
//   - 端侧结果一律 `source_trust=untrusted`（不变量 4）：由派发器强制覆盖，本层不再放宽
//   - 本文件不碰存储、不写 tool_events（落库由引擎按 ToolRunResult 统一写，不变量 1）
//
// 依赖方向：本包只依赖 pkg/toolapi 与 internal/{agent,model}；
// 端侧派发器通过**本包定义的窄接口**注入（cmd 装配处适配 device.Dispatcher），
// 因此 internal/tools 不 import internal/device（保持工具内核可独立替换执行面）。

// DeviceTask 发往端侧的任务（本包窄视图，避免直接依赖 device 包的类型）。
type DeviceTask struct {
	// TaskID 任务 ID（恒等于 ToolEventID，端侧据此幂等）。
	TaskID string
	// RunID 所属 Run。
	RunID string
	// ToolEventID 工具事件 ID（= TaskID）。
	ToolEventID string
	// ToolName 工具名。
	ToolName string
	// Args 工具参数（原样透传到端侧）。
	Args json.RawMessage
	// DeviceID 指定执行设备（空 = 自动挑选在线设备）。
	DeviceID string
	// Timeout 覆盖默认派发超时（0 = 用派发器配置值）。
	Timeout time.Duration
	// OnProgress 进度回调（可选；端侧只产事件，不落库）。
	OnProgress func(chunk string)
}

// DeviceTaskResult 端侧任务终态（收敛后）。
type DeviceTaskResult struct {
	// DeviceID 实际执行设备。
	DeviceID string
	// Status 归一化后的工具事件状态（ok / error / timeout）。
	Status model.ToolEventStatus
	// Result 端侧返回的结果 JSON（成功时）。
	Result json.RawMessage
	// Error 失败原因。
	Error string
	// SourceTrust 来源可信度（端侧恒 untrusted）。
	SourceTrust model.TrustLevel
	// Degraded 端侧不可用，调用方应**静默降级**本地执行（不变量 5）。
	Degraded bool
	// Timeout deadline 到期导致失败（**不降级**：端侧可能已产生副作用）。
	Timeout bool
}

// DeviceExecutor 端侧执行面（`device.Dispatcher` 经 cmd 适配实现；nil = 只用本地执行）。
//
// 返回约定：
//   - degraded=true 的返回值或 `ErrDeviceUnavailable` 错误 → 调用方静默降级本地
//   - 其它 error → 视为端侧异常，同样降级本地（端侧故障不得阻塞 Run）
type DeviceExecutor interface {
	// Execute 派发任务并等待终态。
	Execute(ctx context.Context, task DeviceTask) (DeviceTaskResult, error)
	// Online 当前在线设备数（0 = 端侧不可用，直接跳过远端请求）。
	Online() int
}

// ErrDeviceUnavailable 端侧不可用（本包语义：调用方静默降级本地执行）。
//
// 故意与 device.ErrDeviceUnavailable 区分：本包不感知 device 包，
// 适配层负责把 device 侧错误归一为本错误或 degraded 结果。
var ErrDeviceUnavailable = errors.New("tools: 端侧执行面不可用")

// ErrDeviceTaskInFlight 同一 task_id 已在端侧在途（**禁止**降级本地，否则会重复执行）。
//
// 正常路径不会出现（tool_event_id 每次调用唯一），属于防御性保护。
var ErrDeviceTaskInFlight = errors.New("tools: 端侧任务在途，禁止本地重跑")

// SetDeviceExecutor 注入端侧执行面（**仅装配期调用**；传 nil 等价于关闭端侧委派）。
//
// 之所以用 setter 而非构造函数参数：与既有后置注入范式一致（同 SetDisconnector /
// SetOverrides），且保证未接设备域的既有调用点（含测试）无需改动。
func (r *Runner) SetDeviceExecutor(d DeviceExecutor) { r.devices = d }

// DeviceExecutor 返回当前端侧执行面（未接线为 nil）。
func (r *Runner) DeviceExecutor() DeviceExecutor { return r.devices }

// wantsDevice 判定该工具是否**有可能**在端侧执行（executor ∈ {device, any}）。
//
// 注意：这里只看工具契约的归属，不看端侧是否在线——在线判定在 tryDevice 内做，
// 保证「无设备时零开销跳过」（不产生派发器调用）。
func wantsDevice(spec toolapi.Spec) bool {
	switch spec.Executor {
	case toolapi.ExecutorDevice, toolapi.ExecutorAny:
		return true
	default:
		return false
	}
}

// tryDevice 尝试端侧执行；ok=false 表示应**静默降级**本地执行（不向用户报错）。
func (r *Runner) tryDevice(ctx context.Context, spec toolapi.Spec, req agent.ToolRunRequest) (
	agent.ToolRunResult, bool,
) {
	if r.devices == nil || !wantsDevice(spec) {
		return agent.ToolRunResult{}, false
	}
	// 无在线设备 → 立即降级，不发起派发（不变量 5：绝不让用户等 TTL）
	if r.devices.Online() <= 0 {
		return agent.ToolRunResult{}, false
	}

	res, err := r.devices.Execute(ctx, DeviceTask{
		TaskID:      req.ToolEventID,
		RunID:       req.RunID,
		ToolEventID: req.ToolEventID,
		ToolName:    req.ToolName,
		Args:        json.RawMessage(req.ArgsJSON),
	})
	switch {
	case res.Degraded:
		// 端侧不可用：静默降级（派发器已记 warn 日志，用户侧无感）
		return agent.ToolRunResult{}, false
	case err != nil:
		if errors.Is(err, ErrDeviceTaskInFlight) {
			// 端侧在途：禁止降级本地重跑，明确报错（正常路径不应触发）
			return failure(model.ToolError, err.Error()), true
		}
		if errors.Is(err, ErrDeviceUnavailable) {
			return agent.ToolRunResult{}, false
		}
		if ctx.Err() != nil {
			// 上层取消/超时：按本地同名语义表达（不降级，避免二次副作用）
			return failureForCtx(ctx), true
		}
		// 端侧异常（协议错、帧错等）同样静默降级，保证 Run 不因端侧故障中断
		return agent.ToolRunResult{}, false
	}

	// 端侧真实执行过：结果原样落地，**不重跑**（超时亦不降级，避免重复副作用）
	status := res.Status
	if status == "" {
		status = model.ToolError
	}
	if status != model.ToolOK {
		return agent.ToolRunResult{
			Status:      status,
			Error:       orDefault(res.Error, "端侧执行失败"),
			Executor:    model.ExecutorDevice,
			DeviceID:    res.DeviceID,
			SourceTrust: model.TrustUntrusted,
		}, true
	}

	raw, marshalErr := json.Marshal(devicePayload(res))
	if marshalErr != nil {
		return failure(model.ToolError, "端侧结果序列化失败: "+marshalErr.Error()), true
	}
	return agent.ToolRunResult{
		Status:      model.ToolOK,
		ResultJSON:  string(raw),
		Executor:    model.ExecutorDevice,
		DeviceID:    res.DeviceID,
		SourceTrust: model.TrustUntrusted, // 不变量 4：端侧结果一律不可信，供阶段 F 复检
	}, true
}

// devicePayload 把端侧结果包装成与本地执行**同形状**的合法 JSON。
//
// 与本地路径一致使用 {"content","meta"}：ResultJSON 是 json.RawMessage，
// 直接塞端侧原始结构或纯文本都可能让事件序列化/前端渲染异常（历史踩坑）。
func devicePayload(res DeviceTaskResult) map[string]any {
	content := strings.TrimSpace(string(res.Result))
	if content == "" {
		content = "端侧执行成功（无返回内容）"
	}
	return map[string]any{
		"content": content,
		"meta": map[string]any{
			"executor":     string(model.ExecutorDevice),
			"device_id":    res.DeviceID,
			"source_trust": string(model.TrustUntrusted),
		},
	}
}

// failureForCtx 把 ctx 取消/超时翻译为工具结果（失败一律走 Status，不返回 error）。
func failureForCtx(ctx context.Context) agent.ToolRunResult {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return failure(model.ToolTimeout, "端侧执行超时: "+ctx.Err().Error())
	}
	return failure(model.ToolError, "端侧执行被取消: "+ctx.Err().Error())
}

// orDefault 空串兜底。
func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
