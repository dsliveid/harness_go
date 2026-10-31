package main

import (
	"context"
	"errors"

	"harness-go/internal/agent"
	"harness-go/internal/device"
	"harness-go/internal/events"
	"harness-go/internal/model"
	"harness-go/internal/tools"
)

// ============ C-5 执行面委派适配（cmd 层装配，唯一允许同时看见 tools 与 device 的位置） ============
//
// 为什么放在 cmd：让 internal/tools **不 import internal/device**（工具内核可独立替换执行面），
// 同时让 internal/device **不 import internal/tools**（设备域不感知工具语义）。
// 二者只在装配点交汇，符合「依赖单向向内 + 插件/执行面可替换」。

// deviceTaskExecutor 把 device.Dispatcher 适配成 tools.DeviceExecutor。
type deviceTaskExecutor struct {
	hub        *device.Hub
	dispatcher *device.Dispatcher
	// onProgress 端侧进度回调（端侧只产事件，不落库）。
	onProgress func(runID string, p device.TaskProgressPayload)
}

// Online 当前在线设备数（0 → 执行面路由直接静默降级本地）。
func (e *deviceTaskExecutor) Online() int {
	if e.hub == nil {
		return 0
	}
	return e.hub.Count()
}

// Execute 派发任务并归一化返回值/错误（把 device 域语义翻译成 tools 域语义）。
func (e *deviceTaskExecutor) Execute(ctx context.Context, task tools.DeviceTask) (tools.DeviceTaskResult, error) {
	if e.dispatcher == nil {
		return tools.DeviceTaskResult{Degraded: true}, tools.ErrDeviceUnavailable
	}
	res, err := e.dispatcher.Dispatch(ctx, device.TaskRequest{
		TaskID:      task.TaskID,
		RunID:       task.RunID,
		ToolEventID: task.ToolEventID,
		ToolName:    task.ToolName,
		Args:        task.Args,
		DeviceID:    task.DeviceID,
		Timeout:     task.Timeout,
		OnProgress: func(p device.TaskProgressPayload) {
			if task.OnProgress != nil {
				task.OnProgress(p.Chunk)
				return
			}
			if e.onProgress != nil {
				e.onProgress(p.RunID, p)
			}
		},
	})
	if err != nil {
		// 同一 task_id 已在端侧在途：**禁止**降级本地（否则会重复执行产生二次副作用）
		if errors.Is(err, device.ErrTaskInFlight) {
			return tools.DeviceTaskResult{}, tools.ErrDeviceTaskInFlight
		}
		// 无可用设备 / 写帧失败 → 统一翻译成 tools 域「端侧不可用」（静默降级信号）
		return tools.DeviceTaskResult{Degraded: true, SourceTrust: model.TrustUntrusted}, tools.ErrDeviceUnavailable
	}
	return tools.DeviceTaskResult{
		DeviceID:    res.DeviceID,
		Status:      res.Status,
		Result:      res.Result,
		Error:       res.Error,
		SourceTrust: res.SourceTrust,
		Degraded:    res.Degraded,
		Timeout:     res.Timeout,
	}, nil
}

// 编译期断言：适配层必须满足执行面路由的窄接口。
var _ tools.DeviceExecutor = (*deviceTaskExecutor)(nil)

// newDeviceTaskExecutor 装配执行面委派（未启用设备域时返回 nil，路由自动全走本地）。
func newDeviceTaskExecutor(hub *device.Hub, d *device.Dispatcher, onProgress func(runID string, p device.TaskProgressPayload)) tools.DeviceExecutor {
	if hub == nil || d == nil {
		return nil
	}
	return &deviceTaskExecutor{hub: hub, dispatcher: d, onProgress: onProgress}
}

// publishDeviceProgress 把端侧进度转成 tool.progress 事件（端侧只产事件，落库仍由引擎负责）。
func publishDeviceProgress(pub agent.EventPublisher) func(runID string, p device.TaskProgressPayload) {
	if pub == nil {
		return nil
	}
	return func(runID string, p device.TaskProgressPayload) {
		if runID == "" {
			return
		}
		pub.Publish(runID, events.TypeToolProgress, map[string]any{
			"tool_event_id": p.ToolEventID,
			"chunk":         p.Chunk,
		})
	}
}
