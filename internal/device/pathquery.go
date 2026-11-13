package device

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"harness-go/internal/apperr"
)

// ============ 目录列举 RPC（L1：客户端宿主的目录选择） ============
//
// ## 为什么不用 Dispatcher.Dispatch
//
// 目录浏览在语义上有三处与"工具任务"根本不同（对接约定 §3.4）：
//  1. **非审计**：浏览目录不该产生 tool_event。Dispatcher 的 TaskRequest 里
//     TaskID/ToolEventID 是工具事件的幂等键（落库主键），走它等于让每次点开
//     目录都写一条审计记录，污染 GET /api/v1/audit/tool-events 与日志统计；
//  2. **高频轻量**：用户连点目录能在几百毫秒内打出十几个请求，而 dispatcher
//     的幂等缓存只有 256 条 / 5 分钟，会被目录浏览迅速冲掉，进而让真正的工具
//     任务失去幂等保护（重派时重复执行 = 重复副作用）；
//  3. **不需要重派**：目录列举是纯读且瞬时过期，失败了让用户刷新即可，
//     不像工具任务需要 attempt 递增与终态收敛。
//
// 因此这里实现一个**极简请求-应答 RPC**：只复用 Envelope 帧与 Conn 写出，
// 不碰 dispatcher 的 history / 记账 / 重派。
//
// ## 三条铁律
//
//	A. **强制指定 deviceID**：绝不"换一台在线设备试试"。dispatcher 的 candidates()
//	   会在未指定时挑任一在线设备，那对目录列举是灾难 —— 用户会看到别台机器的盘符。
//	B. **绝不回落服务端本地列举**：设备不可用时必须明确报错。静默降级会把服务端
//	   的目录列进客户端空间，用户以为自己在浏览手机/桌面机，实际看到的是服务器硬盘。
//	C. **不落库**：本文件不 import store / engine，目录浏览不产生任何持久化痕迹。

// ErrPathQueryUnavailable 设备不在线或未注册连接。
var ErrPathQueryUnavailable = errors.New("device: 端侧目录列举不可用")

// pathQueryTimeout 单次列举超时。
//
// 3 秒来自对接约定 §3.3 第 6 条（"单次列举必须在 3 秒内返回"）：桌面端自身给
// 执行器留了 5s，服务端必须比它更早放弃，否则用户要干等桌面端超时后才能看到
// 错误；同时也避免挂死的设备把 HTTP 请求拖到网关超时。
const pathQueryTimeout = 3 * time.Second

// pathQueryMaxPending 同时在途的列举请求上限（防御性上限，避免端侧失联时堆积）。
const pathQueryMaxPending = 64

// pathQuerySeq 请求 ID 的进程级递增值（消除同纳秒碰撞）。
var pathQuerySeq atomic.Uint64

// PathQuery 目录列举 RPC。
type PathQuery struct {
	hub *Hub
	log *slog.Logger

	mu      sync.Mutex
	waiters map[string]chan FsListResultPayload // reqID → 结果通道
}

// NewPathQuery 创建 RPC。hub 为 nil 时所有请求都会明确失败（不放行）。
func NewPathQuery(hub *Hub, log *slog.Logger) *PathQuery {
	return &PathQuery{hub: hub, log: log, waiters: make(map[string]chan FsListResultPayload)}
}

// List 向指定设备请求列举目录。
//
// path 为空串表示"未定位"：端侧应只返回 roots。
//
// 错误口径（与对接约定 §3.2 表格一致，由 api 层映射 HTTP 状态码）：
//   - 设备不在线 / 无连接        → apperr.CodeFSHostOffline
//   - 超时                        → apperr.CodeFSHostTimeout
//   - 端侧回报拒绝（越界/不存在）→ 原样透出端侧 code
func (q *PathQuery) List(ctx context.Context, deviceID, path string) (*FsListResultPayload, error) {
	if q == nil || q.hub == nil {
		return nil, apperr.New(apperr.CodeFSHostUnsupported, "服务端未启用设备域，无法列举客户端目录")
	}
	deviceID = trimSpace(deviceID)
	if deviceID == "" {
		// 铁律 A：绝不靠"挑一台在线设备"来满足请求。
		return nil, apperr.New(apperr.CodeFSHostUnsupported, "必须指定目标设备，不允许改选其他设备")
	}

	c, ok := q.hub.Get(deviceID)
	if !ok || c == nil {
		return nil, apperr.New(apperr.CodeFSHostOffline, "该设备当前不在线，无法列举其目录").WithDetails(map[string]any{
			"device_id": deviceID,
		})
	}

	reqID := newPathQueryID()
	ch := make(chan FsListResultPayload, 1)
	if err := q.register(reqID, ch); err != nil {
		return nil, err
	}
	defer q.unregister(reqID)

	env := MustEnvelope(MsgFsList, reqID, FsListPayload{
		Path:      path,
		WithRoots: path == "",
	})
	if err := c.Send(env); err != nil {
		if q.log != nil {
			q.log.Warn("下发目录列举失败", "device_id", deviceID, "err", err)
		}
		return nil, apperr.New(apperr.CodeFSHostOffline, "向该设备下发目录列举失败，请确认设备连接状态").WithDetails(map[string]any{
			"device_id": deviceID,
		})
	}

	timer := time.NewTimer(pathQueryTimeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return q.translate(res, deviceID)
	case <-timer.C:
		return nil, apperr.New(apperr.CodeFSHostTimeout, "该设备列举目录超时（3 秒），请稍后重试").WithDetails(map[string]any{
			"device_id": deviceID,
		})
	case <-ctx.Done():
		return nil, apperr.New(apperr.CodeFSHostTimeout, "目录列举被取消或超时").WithDetails(map[string]any{
			"device_id": deviceID,
		})
	}
}

// Deliver 由网关在收到 fs.list.result 帧时调用（reqID 配对）。
//
// 返回 false 表示没有等待者（端侧迟到的应答，或请求已超时被清理）——
// 这不是错误，静默丢弃即可，**不得**升级为日志噪音：用户快速翻目录时
// 超时后的迟到应答是常态。
func (q *PathQuery) Deliver(reqID string, payload FsListResultPayload) bool {
	if q == nil || reqID == "" {
		return false
	}
	q.mu.Lock()
	ch, ok := q.waiters[reqID]
	q.mu.Unlock()
	if !ok {
		return false
	}
	// 非阻塞投递：通道容量 1 且只投一次；若已有值（重复应答），丢弃后者。
	select {
	case ch <- payload:
		return true
	default:
		return false
	}
}

// Pending 当前在途请求数（测试与运维观测用）。
func (q *PathQuery) Pending() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiters)
}

// register 登记等待者（含在途上限保护）。
func (q *PathQuery) register(reqID string, ch chan FsListResultPayload) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.waiters) >= pathQueryMaxPending {
		return apperr.New(apperr.CodeFSHostTimeout, "目录列举请求过多，请稍后重试")
	}
	q.waiters[reqID] = ch
	return nil
}

// unregister 清理等待者（幂等）。
func (q *PathQuery) unregister(reqID string) {
	q.mu.Lock()
	delete(q.waiters, reqID)
	q.mu.Unlock()
}

// translate 把端侧结果翻译为「结果或错误」。
//
// 端侧拒绝（越界/不存在）必须如实透出，**不得**降级成空列表：
// 展示空列表会让用户以为"那台机器上这个目录是空的"，从而做出错误判断。
func (q *PathQuery) translate(res FsListResultPayload, deviceID string) (*FsListResultPayload, error) {
	if res.Error != "" || res.Code != "" {
		code := res.Code
		msg := res.Error
		if msg == "" {
			msg = "该设备拒绝了目录列举请求"
		}
		// 🚨 必须用 "%s" 而非直接把 msg 当格式串：端侧文案里可能含 %（如 "50% 完成"），
		// 直接透传会让 Sprintf 解析出 %!(NOVERB) 甚至 panic 级的格式错乱。
		switch code {
		case "path_not_found":
			return nil, apperr.New(apperr.CodeFSPathNotFound, "%s", msg).WithDetails(map[string]any{
				"device_id": deviceID,
			})
		case "path_denied", "":
			return nil, apperr.New(apperr.CodeFSPathDenied, "%s", msg).WithDetails(map[string]any{
				"device_id": deviceID,
			})
		default:
			// 未知端侧码：保守按「拒绝」处理，不回传原始码（避免端侧码泄漏成对外契约）。
			return nil, apperr.New(apperr.CodeFSPathDenied, "%s", msg).WithDetails(map[string]any{
				"device_id": deviceID,
			})
		}
	}
	// 端侧未给 separator 时按空串透出，由前端自行兜底（此处不猜平台：
	// 服务端是 Linux 而设备可能是 Windows，猜错会导致面包屑切分错乱）。
	return &res, nil
}

// newPathQueryID 生成请求 ID。
//
// 只需在「同一服务端进程 + 同一设备」维度内不冲突：纳秒时间戳已足够，
// 再拼一个进程级递增值消除理论上的同纳秒碰撞（不引入随机数，便于测试复现）。
func newPathQueryID() string {
	return fmt.Sprintf("fsq-%d-%d", time.Now().UnixNano(), pathQuerySeq.Add(1))
}

// trimSpace 轻量去空（保持本文件依赖最小，避免误用 strings 的其他语义）。
func trimSpace(s string) string { return strings.TrimSpace(s) }
