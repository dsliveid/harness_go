// 设备状态实时推送（SSE）。
//
// # 与 /runs/{rid}/events 的区别（勿混用）
//
//	| 维度       | /sessions/{sid}/runs/{rid}/events | /api/v1/devices/events      |
//	| ---------- | --------------------------------- | --------------------------- |
//	| 数据性质   | Run 事件流（含 seq，可续传）      | 设备状态**当前态**          |
//	| 续传       | Last-Event-ID 按 seq 补发         | **不续传**，重连即重拿快照  |
//	| 序号       | per-Run 单调递增                  | 无序号（状态无历史意义）    |
//	| 鉴权       | 用户令牌（走全局中间件）          | 用户令牌（同左）            |
//	| 底层总线   | events.Bus（per-Run）             | device.StateBus（独立）     |
//
// 为什么设备状态不做 Last-Event-ID 续传：设备状态是**当前态**而非事件历史 ——
// 断线期间错过 3 次上下线，重连后只需要知道「现在谁在线」，重放中间过程
// 既无意义又会让 UI 短暂显示错误状态（先看到 offline 再看到 online 的抖动）。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/device"
)

// deviceEvents 设备状态 SSE：GET /api/v1/devices/events
//
// 帧格式（与 /runs/{rid}/events 保持一致的 SSE 写法）：
//
//	event: snapshot
//	data: {"type":"snapshot","all":[...],"at":1700000000000}
//
//	event: device.online
//	data: {"type":"device.online","device":{...},"at":...}
//
// 首帧固定是 `snapshot`（全量），随后是增量。前端只需维护一份
// device_id → DeviceState 的映射：snapshot 全量覆盖，增量按 device_id 落位。
func (h *handlers) deviceEvents(w http.ResponseWriter, r *http.Request) {
	if h.d.DeviceState == nil {
		// 设备子系统未装配（未配置 devices 段）：与 /devices/ws 的语义一致，
		// 返回可读的 disabled 错误而不是挂起一个永远不来数据的连接。
		writeError(w, r, apperr.New(apperr.CodeDeviceDisabled, "设备功能未配置"))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		// SSE 必须能逐帧 flush，否则所有事件会堆到连接关闭才一次性吐出，
		// 「实时」语义彻底失效。这里显式拒绝而不是静默降级。
		writeError(w, r, apperr.New(apperr.CodeUnavailable, "当前连接不支持流式推送"))
		return
	}

	sub := h.d.DeviceState.Subscribe()
	defer h.d.DeviceState.Unsubscribe(sub)

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 反代禁用缓冲
	w.WriteHeader(http.StatusOK)

	// ① 首帧：全量快照。
	//
	// 必须在 Subscribe() **之后**读 Snapshot()：反过来的话，两次调用之间
	// 发生的上下线变化会既不在快照里、也不在后续事件里（丢失）。
	// 先订阅再快照，最坏情况是同一变化既出现在快照又在增量里 ——
	// 而前端按 device_id 覆盖写入，重复是**幂等**的，丢事件则不是。
	snapshot := h.d.DeviceState.Snapshot()
	if err := writeDeviceSSE(w, device.StateEventSnapshot, device.DeviceStateEvent{
		Type: device.StateEventSnapshot,
		All:  snapshot,
		At:   time.Now().UnixMilli(),
	}); err != nil {
		return
	}
	flusher.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case ev, open := <-sub.C():
			if !open {
				// 慢消费者被总线断开：不发 resync（设备状态没有历史可对齐），
				// 直接结束，由浏览器 EventSource 自动重连并重新拿快照。
				return
			}
			if err := writeDeviceSSE(w, ev.Type, ev); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			// SSE 注释行保活（不产生事件，只防中间设备超时断开）。
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// writeDeviceSSE 写一帧设备状态 SSE。
func writeDeviceSSE(w http.ResponseWriter, event string, payload device.DeviceStateEvent) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}
