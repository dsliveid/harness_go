// 会话列表实时推送（SSE）：`GET /api/v1/sessions/events`。
//
// # 为什么需要这个端点
//
// 各端（Web / 移动端 / 桌面端）此前只有「打开某个会话内的内容」会自动刷新
// （复用 `/sessions/{sid}/runs/{rid}/events` 的 Run 事件流），而**对话列表本身
// 与每个会话的状态（运行中脉冲点 / 标题自动生成 / 归档 / 删除）**只在页面挂载时
// 拉取一次 `GET /api/v1/sessions`，此后永不刷新 —— 跨端协同时另一端完全静默，
// 必须手动刷新页面才能看到新会话与运行状态。
//
// # 与另外两条 SSE 的区别（三条流勿混用）
//
//	| 维度       | /sessions/{sid}/runs/{rid}/events | /api/v1/devices/events | /api/v1/sessions/events（本端点） |
//	| ---------- | --------------------------------- | ---------------------- | --------------------------------- |
//	| 观测对象   | 单个 Run 的事件流                 | 设备当前态             | 会话**列表**的变更                |
//	| 续传       | Last-Event-ID 按 seq 补发         | 不续传，重连拿快照     | 不续传，重连拿快照                |
//	| 序号       | per-Run 单调递增                  | 无                     | 无                                |
//	| 底层总线   | events.Bus（per-Run）             | device.StateBus        | sessionbus.Bus                    |
//
// # 帧格式
//
//	event: snapshot
//	data: {"type":"snapshot","items":[<会话视图>...],"at":1700000000000}
//
//	event: session.updated
//	data: {"type":"session.updated","session":<会话视图>,"at":...}
//
//	event: session.removed
//	data: {"type":"session.removed","session_id":"s-1","at":...}
//
// 首帧固定是 `snapshot`（全量），随后是增量。
//
// # 为什么事件帧要回查 DB 而不是直接转发总线载荷
//
// 总线只广播 `{type, session_id}`（见 sessionbus 包注释）。handler 收到后**回查
// 数据库**组装权威视图，这样：
//   - 埋点方无需先查会话（写路径极简，不会因查库失败而丢掉广播）；
//   - 推送内容永远是此刻的 DB 真值，不存在「先广播后回滚」造成的陈旧数据；
//   - 帧结构自洽 —— 客户端拿到 `session` 就能直接 upsert，无需二次请求。
//
// 代价是每帧一次按主键查询，且已被总线的 1s 合并窗口摊薄（一次 Run 里几十次
// token 累加最多产生 1 帧）。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/model"
	"harness-go/internal/sessionbus"
)

// sessionEventsSnapshotLimit 快照帧内的会话上限。
//
// 与前端 `api.listSessions({limit:100})` 保持同量级：这是「列表」，不是全库导出。
// 取 200 是给归档中心留余量（归档会话也在同一份列表里，前端自行分区）。
//
// 若用户的会话数超过本上限，多出的部分不会出现在快照里 —— 但这不会造成「丢数据」，
// 因为列表刷新是**覆盖式**的：前端首屏 `GET /sessions` 与快照用同一套 limit 语义时
// 结果一致。真正的分页由前端显式翻页处理，不在实时流的职责范围内。
const sessionEventsSnapshotLimit = 200

// sessionEvents 会话列表 SSE：GET /api/v1/sessions/events
func (h *handlers) sessionEvents(w http.ResponseWriter, r *http.Request) {
	if h.d.SessionEvents == nil {
		// 未装配总线（部分测试 env / 裁剪部署）：明确报可读错误，
		// 而不是挂起一个永远不来数据的连接（同 /devices/events 的口径）。
		writeError(w, r, apperr.New(apperr.CodeUnavailable, "会话实时推送未启用"))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		// SSE 必须能逐帧 flush，否则事件会堆到连接关闭才一次性吐出，「实时」失效。
		writeError(w, r, apperr.New(apperr.CodeUnavailable, "当前连接不支持流式推送"))
		return
	}

	sub := h.d.SessionEvents.Subscribe()
	defer h.d.SessionEvents.Unsubscribe(sub)

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 反代禁用缓冲
	w.WriteHeader(http.StatusOK)

	// ① 首帧：全量快照。
	//
	// 必须在 Subscribe() **之后**查库：反过来的话，两次调用之间发生的变更会
	// 既不在快照里、也不在后续事件里（丢失）。先订阅再快照，最坏情况是同一变化
	// 既出现在快照又在增量里 —— 前端按 session_id 覆盖写入，重复是**幂等**的，
	// 丢事件则不是。这与 device_events_handler.go 的处理完全一致。
	snapshot, err := h.sessionSnapshot(r)
	if err != nil {
		// 快照都拿不到就没必要维持连接：让客户端退避重连后再试。
		writeSessionSSE(w, sessionbus.EventSnapshot, map[string]any{
			"type": sessionbus.EventSnapshot,
			"at":   time.Now().UnixMilli(),
		})
		return
	}
	if err := writeSessionSSE(w, sessionbus.EventSnapshot, map[string]any{
		"type":  sessionbus.EventSnapshot,
		"items": snapshot,
		"at":    time.Now().UnixMilli(),
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
				// 慢消费者被总线断开：不发 resync（本流无历史可对齐），
				// 直接结束，由客户端退避重连并重新拿快照。
				return
			}
			if !h.writeSessionEvent(w, r, ev) {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			// SSE 注释行保活（不产生事件，只防中间设备按空闲超时断开）。
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// writeSessionEvent 处理一条总线事件并写帧；返回 false 表示应结束连接。
func (h *handlers) writeSessionEvent(w http.ResponseWriter, r *http.Request, ev sessionbus.Event) bool {
	switch ev.Type {
	case sessionbus.EventRemoved:
		// 删除是**终态**：不查库（会话已不存在，查也是 not found），直接告知 id。
		if err := writeSessionSSE(w, sessionbus.EventRemoved, map[string]any{
			"type":       sessionbus.EventRemoved,
			"session_id": ev.SessionID,
			"at":         ev.At,
		}); err != nil {
			return false
		}
		return true

	case sessionbus.EventUpdated:
		sess, err := h.d.Sessions.Get(r.Context(), ev.SessionID)
		if err != nil || sess == nil {
			// 会话已被删除或不可读（并发删除的竞态）：静默跳过本帧。
			//
			// 不改成发 removed 的原因：这里无法区分「刚被删除」与「瞬时查询失败」，
			// 误发 removed 会让客户端永久摘掉一个仍存在的会话（比不更新更糟）。
			// 真正的删除会有独立的 removed 事件（PublishRemoved）。
			return true
		}
		if err := writeSessionSSE(w, sessionbus.EventUpdated, map[string]any{
			"type":    sessionbus.EventUpdated,
			"session": sess,
			"at":      ev.At,
		}); err != nil {
			return false
		}
		return true

	default:
		// 领域列表变更（projects.changed / workspaces.changed）与未来新增事件：
		// 都是**纯信号**（不带数据）—— 原样转发一帧，不回查 DB。
		// 前端收到后自行整表重拉，因此这里没有任何「查不到实体」的竞态问题。
		if err := writeSessionSSE(w, ev.Type, map[string]any{
			"type": ev.Type,
			"at":   ev.At,
		}); err != nil {
			return false
		}
		return true
	}
}

// sessionSnapshot 查询会话列表作为首帧快照。
func (h *handlers) sessionSnapshot(r *http.Request) ([]model.Session, error) {
	return h.d.Sessions.List(r.Context(), sessionEventsSnapshotLimit, 0)
}

// writeSessionSSE 写一帧会话列表 SSE。
func writeSessionSSE(w http.ResponseWriter, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}
