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

// pathQueryFailureMaxDevices 失败快照最多保留的设备数。
//
// 防御性上限：设备很多（几百台）时也不会让进程内 map 无限增长。超额时按
// 「最久未失败者优先」清理，见 evictFailuresLocked。
const pathQueryFailureMaxDevices = 256

// pathQueryFailureTTL 失败快照条目的惰性过期时长。
//
// 超过该时长未再失败即视为陈旧条目，容量吃紧时优先丢弃。取 10 分钟是因为它只
// 决定「连续超时」的判定窗口，而用户重新点开目录是分钟级动作；不做周期性 GC
// 协程（与 waiters 同风格：只在写入点顺手清理）。
const pathQueryFailureTTL = 10 * time.Minute

// pathQueryLateReqMax 迟到应答定位表容量（reqID → deviceID）。
//
// 只为回答一个问题：「这个 reqID 的超时是发给哪台设备的」。用户快速翻目录时
// 超时与迟到应答交替出现是常态，因此保留最近若干个已超时的 reqID 供 Deliver 失配时反查。
const pathQueryLateReqMax = 128

// pathQueryLateReqTTL 迟到应答定位的有效期（超过即不可能再等到应答）。
const pathQueryLateReqTTL = time.Minute

// PathQueryFailureState 单设备失败快照的只读视图（测试与运维观测用）。
type PathQueryFailureState struct {
	DeviceID      string
	Consecutive   int
	SawLateReply  bool
	LastFailureAt time.Time
}

// pathQueryFailure 单设备的连续失败快照（进程内、**不落库**：铁律 C）。
//
// 存在的唯一理由：把「端侧还没实现 fs.list（帧被 serde 归为 Unknown 静默丢弃）」
// 与「端侧活着但这次慢了」区分开。两者在服务端看来都只是「3 秒没回帧」，
// 但前者重试一万次也不会好，后者重试一次就好 —— 必须给出不同文案与不同可重试性。
type pathQueryFailure struct {
	// consecutive 连续超时次数（任何一次收到应答即清零）
	consecutive int
	// sawLateReply 期间是否收到过迟到应答：收到说明端侧确实活着且实现了 fs.list
	sawLateReply bool
	// lastAt 最近一次失败时间（惰性清理用）
	lastAt time.Time
}

// pathQueryWaiter 一个在途等待者。
//
// 只存通道：失败快照按 deviceID 归集（List 的超时分支手上已有 deviceID），
// 无需在此冗余一份，避免两处口径漂移。
type pathQueryWaiter struct {
	ch chan FsListResultPayload
}

// pathQueryLateReq 已超时请求的定位表条目。
type pathQueryLateReq struct {
	deviceID string
	at       time.Time
}

// PathQuery 目录列举 RPC。
type PathQuery struct {
	hub *Hub
	log *slog.Logger

	mu      sync.Mutex
	waiters map[string]pathQueryWaiter // reqID → 等待者

	// failMu 独立保护失败快照（与 waiters 分开，避免 Deliver 热路径争用同一把锁）
	failMu sync.Mutex
	// failures deviceID → 连续失败快照
	failures map[string]*pathQueryFailure
	// lateReqs 已超时 reqID → 所属设备（供 Deliver 失配时判定「迟到应答」）
	lateReqs map[string]pathQueryLateReq
}

// NewPathQuery 创建 RPC。hub 为 nil 时所有请求都会明确失败（不放行）。
func NewPathQuery(hub *Hub, log *slog.Logger) *PathQuery {
	return &PathQuery{
		hub:      hub,
		log:      log,
		waiters:  make(map[string]pathQueryWaiter),
		failures: make(map[string]*pathQueryFailure),
		lateReqs: make(map[string]pathQueryLateReq),
	}
}

// classifyPathQueryFailure 把「连续超时次数 + 是否收到过迟到应答」归类为对外错误码。
//
// 这是**唯一**的判据出口：handler 里严禁再写 if 分支（否则判据会随改动漂移成两套）。
//
// 判据（顺序即优先级）：
//  1. 出现过迟到应答 → 始终 fs.host_timeout。
//     端侧确实处理了 fs.list（只是超过了 3 秒），说明协议壳已实现，属「慢」而非
//     「不支持」；此时报「不支持」会把真实存在的慢通道永久判死，用户再也不重试。
//  2. 第 1 次超时 → fs.host_timeout（可重试）。
//     偶发抖动（端侧刚启动、reconnect、GC 卡顿）必须给一次重试机会，
//     单次失败就断言「未实现」会造成误判。
//  3. 同设备连续 ≥2 次超时且期间**从未**收到任何迟到应答 → fs.host_unsupported。
//     端侧帧被 serde 归为 Unknown 静默丢弃时，服务端看到的永远是「3 秒无回帧」，
//     而且永远等不到任何迟到应答 —— 这个组合才是「客户端未实现目录列举」的判别式。
//     不可重试：重试只会让用户在白屏前反复等 3 秒。
func classifyPathQueryFailure(consecutive int, sawLateReply bool) apperr.Code {
	if sawLateReply {
		return apperr.CodeFSHostTimeout
	}
	if consecutive >= 2 {
		return apperr.CodeFSHostUnsupported
	}
	return apperr.CodeFSHostTimeout
}

// pathQueryFailureMessage 归类结果对应的对外文案（两档，措辞刻意区分可重试性）。
//
//   - host_timeout 档：强调「稍后重试」，暗示临时故障；
//   - host_unsupported 档：明确「未实现 / 请升级」，消除「再试一次就好了」的误导。
func pathQueryFailureMessage(code apperr.Code) string {
	if code == apperr.CodeFSHostUnsupported {
		return "该客户端未实现目录列举（fs.list），请升级桌面端或手动填写路径"
	}
	return "该设备列举目录超时（3 秒），请稍后重试"
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
			q.log.Warn("下发目录列举失败", "device_id", deviceID, "req_id", reqID, "err", err)
		}
		return nil, apperr.New(apperr.CodeFSHostOffline, "向该设备下发目录列举失败，请确认设备连接状态").WithDetails(map[string]any{
			"device_id": deviceID,
		})
	}

	timer := time.NewTimer(pathQueryTimeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		// 收到任何应答（含端侧拒绝）都说明协议壳已实现且端侧活着 → 清零失败快照。
		q.noteReply(deviceID)
		return q.translate(res, deviceID)
	case <-timer.C:
		return nil, q.failTimeout(deviceID, reqID, "timer")
	case <-ctx.Done():
		return nil, q.failTimeout(deviceID, reqID, "ctx")
	}
}

// failTimeout 超时分支的统一出口：记快照 → 归类 → 打 Warn 日志 → 构造错误。
//
// 抽成一个函数是为了让「timer 超时」与「ctx 取消」两条路径的判据、日志、文案
// 永远一致（此前两条分支各写一份，极易漂移）。
func (q *PathQuery) failTimeout(deviceID, reqID, reason string) error {
	consecutive, sawLate := q.noteTimeout(reqID, deviceID)
	code := classifyPathQueryFailure(consecutive, sawLate)
	// 超时分支此前完全无日志 —— 这是「排障全靠猜」的直接原因，必须补齐。
	q.logWarn("目录列举超时",
		"device_id", deviceID,
		"req_id", reqID,
		"reason", reason,
		"consecutive", consecutive,
		"saw_late_reply", sawLate,
		"classified", string(code),
	)
	return apperr.New(code, "%s", pathQueryFailureMessage(code)).WithDetails(map[string]any{
		"device_id":   deviceID,
		"req_id":      reqID,
		"consecutive": consecutive,
	})
}

// logWarn 空 logger 安全告警（nil 桩与测试环境不需要日志）。
func (q *PathQuery) logWarn(msg string, args ...any) {
	if q == nil || q.log == nil {
		return
	}
	q.log.Warn(msg, args...)
}

// Deliver 由网关在收到 fs.list.result 帧时调用（reqID 配对）。
//
// 返回 false 表示没有等待者（端侧迟到的应答，或请求已超时被清理）——
// 这不是错误，静默丢弃即可，**不得**升级为日志噪音：用户快速翻目录时
// 超时后的迟到应答是常态。
//
// ⚠️ 但「迟到」本身是极高价值的事实：它证明端侧**确实实现了 fs.list**（只是慢），
// 因此这里必须在失配时反查 lateReqs 并给对应设备打上 sawLateReply 标记，
// 否则连续超时会把它误判成「客户端未实现目录列举」。
func (q *PathQuery) Deliver(reqID string, payload FsListResultPayload) bool {
	if q == nil || reqID == "" {
		return false
	}
	q.mu.Lock()
	w, ok := q.waiters[reqID]
	q.mu.Unlock()
	if !ok {
		// 迟到应答（含重复应答）：按 reqID 反查设备并标记，绝不静默丢事实。
		q.noteLateReply(reqID)
		return false
	}
	// 非阻塞投递：通道容量 1 且只投一次；若已有值（重复应答），丢弃后者。
	select {
	case w.ch <- payload:
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

// FailureSnapshot 读取单设备失败快照（测试与运维观测用）。
//
// 返回的 ok=false 表示该设备没有失败记录（例如从未超时过）。
func (q *PathQuery) FailureSnapshot(deviceID string) (PathQueryFailureState, bool) {
	if q == nil {
		return PathQueryFailureState{}, false
	}
	q.failMu.Lock()
	defer q.failMu.Unlock()
	f, ok := q.failures[deviceID]
	if !ok || f == nil {
		return PathQueryFailureState{}, false
	}
	return PathQueryFailureState{
		DeviceID:      deviceID,
		Consecutive:   f.consecutive,
		SawLateReply:  f.sawLateReply,
		LastFailureAt: f.lastAt,
	}, true
}

// ResetFailureState 清空全部失败快照（测试用；生产侧不需要手动重置）。
func (q *PathQuery) ResetFailureState() {
	if q == nil {
		return
	}
	q.failMu.Lock()
	q.failures = make(map[string]*pathQueryFailure)
	q.lateReqs = make(map[string]pathQueryLateReq)
	q.failMu.Unlock()
}

// noteTimeout 记录一次超时，返回（连续次数, 是否曾收到迟到应答）。
//
// 迟到应答定位：超时的 reqID 必须留档，否则端侧稍后回的应答无人可归因。
func (q *PathQuery) noteTimeout(reqID, deviceID string) (int, bool) {
	q.failMu.Lock()
	defer q.failMu.Unlock()
	now := time.Now()
	q.pruneFailuresLocked(now)

	f := q.failures[deviceID]
	if f == nil {
		if len(q.failures) >= pathQueryFailureMaxDevices {
			q.evictFailuresLocked(now)
		}
		f = &pathQueryFailure{}
		q.failures[deviceID] = f
	}
	f.consecutive++
	f.lastAt = now

	if reqID != "" {
		if len(q.lateReqs) >= pathQueryLateReqMax {
			q.pruneLateReqsLocked(now, pathQueryLateReqMax/2)
		}
		q.lateReqs[reqID] = pathQueryLateReq{deviceID: deviceID, at: now}
	}
	return f.consecutive, f.sawLateReply
}

// noteReply 收到正常应答：清零该设备的连续失败计数，并结束当前观察期。
//
// ⚠️ sawLateReply 也必须随之清零（而不是永久保留）：它的语义是「**本次连续失败
// 期间**是否见过迟到应答」。一次成功应答已经是更强的「端侧活着」证据，此时若不
// 清零，设备一旦升级/退化回「完全不回帧」就会永远停留在可重试档，用户被无限
// 劝去重试（3s × N 次）—— 那正是本次重构要消灭的体验。
func (q *PathQuery) noteReply(deviceID string) {
	if q == nil {
		return
	}
	q.failMu.Lock()
	defer q.failMu.Unlock()
	if f := q.failures[deviceID]; f != nil {
		f.consecutive = 0
		f.sawLateReply = false
	}
}

// noteLateReply 迟到应答：把「端侧其实活着」这一事实记到对应设备上。
//
// 迟到的 reqID 若已被清理（超过 TTL/容量），则无法归因，静默忽略——
// 这是可接受的降级：下一次正常的超时-迟到对会重新建立标记。
func (q *PathQuery) noteLateReply(reqID string) {
	if q == nil {
		return
	}
	q.failMu.Lock()
	defer q.failMu.Unlock()
	late, ok := q.lateReqs[reqID]
	if !ok {
		return
	}
	delete(q.lateReqs, reqID)
	if f := q.failures[late.deviceID]; f != nil {
		f.sawLateReply = true
	}
}

// pruneFailuresLocked 惰性清理过期失败快照（调用方已持锁）。
func (q *PathQuery) pruneFailuresLocked(now time.Time) {
	for id, f := range q.failures {
		if f == nil || now.Sub(f.lastAt) > pathQueryFailureTTL {
			delete(q.failures, id)
		}
	}
}

// evictFailuresLocked 容量吃紧时丢弃最久未失败者（调用方已持锁）。
func (q *PathQuery) evictFailuresLocked(now time.Time) {
	for id, f := range q.failures {
		if f == nil || now.Sub(f.lastAt) > pathQueryFailureTTL {
			delete(q.failures, id)
		}
	}
	// 仍然超额：丢最旧的（O(n) 找最小，容量上限 256，成本可忽略）。
	for len(q.failures) >= pathQueryFailureMaxDevices {
		var oldestID string
		var oldest time.Time
		first := true
		for id, f := range q.failures {
			if first || f.lastAt.Before(oldest) {
				oldestID, oldest, first = id, f.lastAt, false
			}
		}
		if oldestID == "" {
			return
		}
		delete(q.failures, oldestID)
	}
}

// pruneLateReqsLocked 惰性清理超时定位表（调用方已持锁）。
//
// keep 为保留上限：先按 TTL 清，再在超额时按时间清到 keep 条以内。
func (q *PathQuery) pruneLateReqsLocked(now time.Time, keep int) {
	for id, r := range q.lateReqs {
		if now.Sub(r.at) > pathQueryLateReqTTL {
			delete(q.lateReqs, id)
		}
	}
	for len(q.lateReqs) > keep {
		var oldestID string
		var oldest time.Time
		first := true
		for id, r := range q.lateReqs {
			if first || r.at.Before(oldest) {
				oldestID, oldest, first = id, r.at, false
			}
		}
		if oldestID == "" {
			return
		}
		delete(q.lateReqs, oldestID)
	}
}

// register 登记等待者（含在途上限保护）。
func (q *PathQuery) register(reqID string, ch chan FsListResultPayload) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.waiters) >= pathQueryMaxPending {
		return apperr.New(apperr.CodeFSHostTimeout, "目录列举请求过多，请稍后重试")
	}
	q.waiters[reqID] = pathQueryWaiter{ch: ch}
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
