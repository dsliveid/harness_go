package device

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/model"
)

// ============ 任务派发器（阶段 C-4） ============
//
// 职责边界（严格遵守既定不变量，勿扩展）：
//   - 只做「派发 → 进度 → 终态收敛」，**绝不落库**（tool_events 由引擎写；
//     端侧永不直写 DB = 不变量 1）
//   - `task_id` 恒等于 `tool_event_id`：端侧据此做幂等去重，服务端据此收敛结果
//     （断线重派、结果重复上报都安全）
//   - 端侧结果一律 `source_trust=untrusted`（不变量 4）：端侧自述的 trusted 被强制覆盖，
//     供阶段 F 复检
//   - 端侧不在线 / 写失败 → **立即**返回 Degraded 结果，绝不等 deadline（不变量 5：
//     静默降级，不向用户报错）
//   - deadline 到期 → Status=timeout，绝不悬空等待
//   - 任务族帧（progress / result）由本文件作为网关 FrameHandler 消费

// 端侧任务终态字符串（TaskResultPayload.Status 的冻结取值，禁止扩展语义）。
const (
	// TaskStatusSuccess 端侧执行成功。
	TaskStatusSuccess = "success"
	// TaskStatusFailed 端侧执行失败。
	TaskStatusFailed = "failed"
	// TaskStatusCancelled 端侧主动取消（端侧被用户中断）。
	TaskStatusCancelled = "cancelled"
)

// 派发失败原因（写入 TaskResult.Error，便于审计与前端提示；不含用户可见措辞）。
const (
	// TaskErrorUnavailable 无在线执行设备（调用方应静默降级本地执行）。
	TaskErrorUnavailable = "device_unavailable"
	// TaskErrorOffline 派发后设备离线/断连。
	TaskErrorOffline = "device_offline"
	// TaskErrorTimeout 超过 deadline 未回终态。
	TaskErrorTimeout = "device_timeout"
	// TaskErrorSendFailed 帧写出失败（通常是端侧已断开）。
	TaskErrorSendFailed = "device_send_failed"
	// TaskErrorMalformed 端侧回报的终态不完整。
	TaskErrorMalformed = "device_bad_result"
)

// 派发默认参数。
const (
	// DefaultTaskTTL 任务派发默认超时（对齐 config.devices.task_ttl_s 缺省值 600s）。
	DefaultTaskTTL = 600 * time.Second
	// dispatchHistoryLimit 幂等缓存条目上限（超出按最旧淘汰）。
	dispatchHistoryLimit = 256
	// dispatchHistoryTTL 幂等缓存存活时长（到期自动失效，允许同 task_id 重派）。
	dispatchHistoryTTL = 5 * time.Minute
	// pendingProgressLimit 单任务保留的进度块上限（防止长任务内存膨胀）。
	pendingProgressLimit = 32
)

// ErrDeviceUnavailable 无可用在线执行设备（调用方据此静默降级本地执行，不向用户报错）。
var ErrDeviceUnavailable = errors.New("device: 无在线执行设备")

// ErrTaskInFlight 同一 task_id 正在执行中（服务端幂等保护，避免重复执行）。
var ErrTaskInFlight = errors.New("device: 同一 task_id 正在执行")

// TaskRequest 任务派发请求（由执行面路由 tools/device_runner 构造）。
type TaskRequest struct {
	// TaskID 任务 ID，**必须等于 ToolEventID**（为空时从 ToolEventID 推导）。
	TaskID string
	// RunID 所属 Run。
	RunID string
	// ToolEventID 工具事件 ID（幂等键，落库主键）。
	ToolEventID string
	// ToolName 工具名（端侧据此分发实现）。
	ToolName string
	// Args 工具参数（原样透传，合法性在此校验）。
	Args json.RawMessage
	// DeviceID 指定执行设备（空 = 自动挑选在线设备）。
	DeviceID string
	// Timeout 覆盖默认 TaskTTL（0 = 用 Dispatcher 配置值）。
	Timeout time.Duration
	// OnProgress 进度回调（可选；为 nil 时用 Dispatcher 级回调）。
	OnProgress func(TaskProgressPayload)
}

// normalized 补全幂等键（task_id 与 tool_event_id 互为兜底）。
func (r TaskRequest) normalized() TaskRequest {
	if r.TaskID == "" {
		r.TaskID = strings.TrimSpace(r.ToolEventID)
	}
	if r.ToolEventID == "" {
		r.ToolEventID = r.TaskID
	}
	return r
}

// TaskResult 任务终态收敛结果。
type TaskResult struct {
	// TaskID 任务 ID（= tool_event_id）。
	TaskID string
	// DeviceID 实际执行设备（降级时为空）。
	DeviceID string
	// Status 归一化后的工具事件状态（ok / error / timeout）。
	Status model.ToolEventStatus
	// Result 端侧返回的结果 JSON（成功时）。
	Result json.RawMessage
	// Error 失败原因（TaskError* 或端侧自报原因）。
	Error string
	// SourceTrust 来源可信度：端侧结果恒 untrusted，降级时亦为 untrusted（无结果可比）。
	SourceTrust model.TrustLevel
	// Degraded 端侧不可用，调用方应静默降级本地执行（不变量 5）。
	Degraded bool
	// Timeout 是否因 deadline 到期而失败。
	Timeout bool
	// Attempt 实际派发次数（>=1；选中设备写失败会换设备重试）。
	Attempt int
	// Duration 端侧实际耗时（从派发到收敛）。
	Duration time.Duration
}

// DevicePickFunc 选择执行设备的策略（C-5 会按能力/负载实现；nil 时取首个在线设备）。
//
// 返回 ok=false 表示「无可派发设备」；返回的设备若已离线，派发器会继续尝试其它在线设备。
type DevicePickFunc func(ctx context.Context, req TaskRequest) (deviceID string, ok bool)

// DispatcherOptions 派发器依赖。
type DispatcherOptions struct {
	Log *slog.Logger
	// Hub 在线连接注册表（必填）。
	Hub *Hub
	// TaskTTL 默认派发超时（<=0 回退 DefaultTaskTTL）。
	TaskTTL time.Duration
	// Pick 设备选择策略（nil = 取首个在线设备，按 ID 排序保证稳定）。
	Pick DevicePickFunc
	// OnProgress 进度回调（兜底 TaskRequest.OnProgress；C-5 接事件总线）。
	OnProgress func(ctx context.Context, deviceID string, p TaskProgressPayload)
	// Now 时钟注入（测试用；缺省 time.Now）。
	Now func() time.Time
}

// Dispatcher 端侧任务派发器（实现网关 FrameHandler 与待派发计数）。
type Dispatcher struct {
	log        *slog.Logger
	hub        *Hub
	taskTTL    time.Duration
	pick       DevicePickFunc
	onProgress func(ctx context.Context, deviceID string, p TaskProgressPayload)
	now        func() time.Time

	mu      sync.Mutex
	pending map[string]*pendingTask
	history map[string]historyEntry
	closed  bool
}

// pendingTask 在途任务的收敛状态（ch 缓冲 1，终态只写一次）。
type pendingTask struct {
	taskID      string
	runID       string
	toolEventID string
	deviceID    string
	attempt     int
	started     time.Time
	deadline    time.Time
	onProgress  func(TaskProgressPayload)

	progress []TaskProgressPayload

	once sync.Once
	ch   chan TaskResult
}

// finish 收敛终态（幂等：重复调用只有首次生效）；返回本次写入的结果，
// 若已被更早的结果抢先收敛，则返回零值 TaskResult（TaskID 为空）。
func (t *pendingTask) finish(res TaskResult) TaskResult {
	var out TaskResult
	t.once.Do(func() {
		out = res
		select {
		case t.ch <- res:
		default:
			// ch 容量 1 且只会写一次，理论不可达；兜底避免阻塞
		}
	})
	return out
}

// historyEntry 幂等缓存条目。
type historyEntry struct {
	res TaskResult
	at  time.Time
}

// NewDispatcher 创建派发器。
func NewDispatcher(opts DispatcherOptions) *Dispatcher {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ttl := opts.TaskTTL
	if ttl <= 0 {
		ttl = DefaultTaskTTL
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Dispatcher{
		log:        log,
		hub:        opts.Hub,
		taskTTL:    ttl,
		pick:       opts.Pick,
		onProgress: opts.OnProgress,
		now:        now,
		pending:    make(map[string]*pendingTask),
		history:    make(map[string]historyEntry),
	}
}

// TaskTTL 默认派发超时。
func (d *Dispatcher) TaskTTL() time.Duration { return d.taskTTL }

// Dispatch 派发任务并等待终态。
//
// 返回约定（调用方据此实现静默降级，勿在 UI 暴露错误）：
//   - 成功派发 → (TaskResult, nil)；Status ∈ {ok, error, timeout}
//   - 无在线设备 / 全部写失败 → (TaskResult{Degraded:true}, ErrDeviceUnavailable)
//   - 同一 task_id 在途 → (TaskResult{}, ErrTaskInFlight)
//   - ctx 取消 → (TaskResult{}, ctx.Err())
//   - 参数非法 → apperr（CodeInvalidArgument / CodeToolBadArgs）
func (d *Dispatcher) Dispatch(ctx context.Context, req TaskRequest) (TaskResult, error) {
	req = req.normalized()
	if req.TaskID == "" {
		return TaskResult{}, apperr.New(apperr.CodeInvalidArgument, "任务缺少 task_id")
	}
	if len(req.Args) > 0 && !json.Valid(req.Args) {
		return TaskResult{}, apperr.New(apperr.CodeToolBadArgs, "工具参数不是合法 JSON")
	}
	// 幂等：已收敛过的同一 task_id 直接复用终态，不再派发（断线重派安全）
	if res, ok := d.cached(req.TaskID); ok {
		return res, nil
	}
	if d.isClosed() {
		return TaskResult{TaskID: req.TaskID, Degraded: true, Error: TaskErrorUnavailable},
			ErrDeviceUnavailable
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = d.taskTTL
	}
	now := d.now()
	task := &pendingTask{
		taskID:      req.TaskID,
		runID:       req.RunID,
		toolEventID: req.ToolEventID,
		started:     now,
		deadline:    now.Add(timeout),
		onProgress:  req.OnProgress,
		ch:          make(chan TaskResult, 1),
	}
	if err := d.register(task); err != nil {
		return TaskResult{}, err
	}
	defer d.removePending(req.TaskID)

	sent, lastErr := d.sendToCandidates(ctx, task, req)
	if !sent {
		reason := TaskErrorSendFailed
		if lastErr == nil {
			reason = TaskErrorUnavailable
		}
		d.log.Warn("端侧任务派发失败，转静默降级",
			"task_id", req.TaskID, "run_id", req.RunID, "tool", req.ToolName, "reason", reason)
		return TaskResult{
			TaskID:      req.TaskID,
			DeviceID:    task.deviceID,
			Status:      model.ToolError,
			Error:       reason,
			SourceTrust: model.TrustUntrusted,
			Degraded:    true,
			Attempt:     task.attempt,
		}, ErrDeviceUnavailable
	}

	remaining := task.deadline.Sub(d.now())
	if remaining < 0 {
		remaining = 0
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()

	select {
	case res := <-task.ch:
		d.record(res)
		return res, nil
	case <-timer.C:
		return d.concludeTimeout(task), nil
	case <-ctx.Done():
		// 上层 Run 被取消：不判端侧失败，交由执行面按 cancelled 处理
		return TaskResult{}, ctx.Err()
	}
}

// sendToCandidates 依次尝试候选设备，返回是否成功写出派发帧。
func (d *Dispatcher) sendToCandidates(ctx context.Context, task *pendingTask, req TaskRequest) (bool, error) {
	cands := d.candidates(ctx, req)
	if len(cands) == 0 {
		return false, nil
	}
	var lastErr error
	attempt := 0
	for _, devID := range cands {
		conn, ok := d.hub.Get(devID)
		if !ok {
			// 刚离线：立即换下一台，不等 deadline（不变量 5）
			continue
		}
		attempt++
		env := MustEnvelope(MsgTaskDispatch, req.TaskID, TaskDispatchPayload{
			TaskID:      req.TaskID,
			RunID:       req.RunID,
			ToolEventID: req.ToolEventID,
			ToolName:    req.ToolName,
			Args:        req.Args,
			Deadline:    task.deadline.UnixMilli(),
			Attempt:     attempt,
		})
		// 信封层也带 run_id（协议形状已冻结，两处冗余便于端侧路由）
		env.RunID = req.RunID

		d.mu.Lock()
		task.deviceID = devID
		task.attempt = attempt
		d.mu.Unlock()

		if err := conn.Send(env); err != nil {
			lastErr = err
			d.log.Warn("任务派发帧写出失败，尝试下一台设备",
				"task_id", req.TaskID, "device_id", devID, "err", err)
			continue
		}
		return true, nil
	}
	return false, lastErr
}

// candidates 计算候选设备列表（稳定顺序：指定设备优先，其次按 ID 排序的在线设备）。
func (d *Dispatcher) candidates(ctx context.Context, req TaskRequest) []string {
	if d.hub == nil {
		return nil
	}
	if id := strings.TrimSpace(req.DeviceID); id != "" {
		if d.hub.Online(id) {
			return []string{id}
		}
		return nil
	}
	if d.pick != nil {
		if id, ok := d.pick(ctx, req); ok {
			if d.hub.Online(id) {
				return []string{id}
			}
			// 策略选中的设备已离线：退化为「任一在线设备」，避免把可派发任务误降级
			d.log.Debug("策略选中的设备已离线，回退到其他在线设备",
				"task_id", req.TaskID, "picked", id)
		}
	}
	ids := d.hub.IDs()
	sort.Strings(ids)
	return ids
}

// concludeTimeout deadline 到期收敛为失败（若真实结果已抢先收敛，以真实结果为准）。
func (d *Dispatcher) concludeTimeout(task *pendingTask) TaskResult {
	timeoutRes := TaskResult{
		TaskID:      task.taskID,
		Status:      model.ToolTimeout,
		Error:       TaskErrorTimeout,
		SourceTrust: model.TrustUntrusted,
		Timeout:     true,
	}
	d.mu.Lock()
	deviceID, attempt, started := task.deviceID, task.attempt, task.started
	d.mu.Unlock()
	timeoutRes.DeviceID = deviceID
	timeoutRes.Attempt = attempt
	timeoutRes.Duration = d.now().Sub(started)

	res := task.finish(timeoutRes)
	if res.TaskID == "" {
		// 端侧结果已在此之前收敛：以端侧结果为准（时序竞态下不丢成功结果）
		select {
		case r := <-task.ch:
			res = r
		default:
			res = timeoutRes
		}
	}
	d.log.Warn("端侧任务超过 deadline，判定失败",
		"task_id", task.taskID, "run_id", task.runID, "device_id", res.DeviceID,
		"timeout_ms", int(d.taskTTL/time.Millisecond))
	d.record(res)
	return res
}

// HandleFrame 实现网关 FrameHandler：消费 task.progress / task.result 上行帧。
//
// 幂等铁律：未知 task_id 的迟到结果**静默忽略**（端侧重放/断线重派后的迟到帧），
// 绝不因未知任务返回错误帧打断连接。
func (d *Dispatcher) HandleFrame(_ context.Context, c *Conn, env Envelope) (fatal bool, err error) {
	switch env.Type {
	case MsgTaskProgress:
		var p TaskProgressPayload
		if dErr := env.Decode(&p); dErr != nil {
			return false, apperr.New(apperr.CodeInvalidArgument, "task.progress 帧负载非法: %s", dErr.Error())
		}
		d.applyProgress(frameDeviceID(c), p)
		return false, nil

	case MsgTaskResult:
		var p TaskResultPayload
		if dErr := env.Decode(&p); dErr != nil {
			return false, apperr.New(apperr.CodeInvalidArgument, "task.result 帧负载非法: %s", dErr.Error())
		}
		d.applyResult(frameDeviceID(c), p)
		return false, nil
	}
	return false, apperr.New(apperr.CodeInvalidArgument, "非任务族帧不应交给派发器：%s", env.Type)
}

// frameDeviceID 从连接取设备 ID（c 为 nil 时返回空，便于单测直接构造帧）。
func frameDeviceID(c *Conn) string {
	if c == nil {
		return ""
	}
	return c.DeviceID()
}

// applyProgress 记录进度并回调（端侧只产事件，不落库）。
func (d *Dispatcher) applyProgress(deviceID string, p TaskProgressPayload) {
	taskID := strings.TrimSpace(p.TaskID)
	if taskID == "" {
		taskID = strings.TrimSpace(p.ToolEventID)
	}
	d.mu.Lock()
	task, ok := d.pending[taskID]
	var cb func(TaskProgressPayload)
	if ok {
		if len(task.progress) < pendingProgressLimit {
			task.progress = append(task.progress, p)
		}
		cb = task.onProgress
	}
	d.mu.Unlock()

	if !ok {
		// 迟到进度：忽略（不视为错误）
		d.log.Debug("收到已完成任务的迟到进度，忽略", "device_id", deviceID, "task_id", taskID)
		return
	}
	if cb != nil {
		cb(p)
		return
	}
	if d.onProgress != nil {
		d.onProgress(context.Background(), deviceID, p)
	}
}

// applyResult 收敛终态（端侧结果强制 untrusted）。
func (d *Dispatcher) applyResult(deviceID string, p TaskResultPayload) {
	taskID := strings.TrimSpace(p.TaskID)
	if taskID == "" {
		taskID = strings.TrimSpace(p.ToolEventID)
	}
	d.mu.Lock()
	task, ok := d.pending[taskID]
	var expected string
	var attempt int
	var started time.Time
	if ok {
		expected, attempt, started = task.deviceID, task.attempt, task.started
	}
	d.mu.Unlock()

	if !ok {
		// 幂等：重复上报 / 迟到结果一律忽略（服务端已收敛的终态为准）
		d.log.Debug("收到已完成任务的迟到结果，忽略（幂等）",
			"device_id", deviceID, "task_id", taskID)
		return
	}
	if expected != "" && deviceID != "" && expected != deviceID {
		d.log.Warn("任务结果来自非派发设备，已拒绝",
			"task_id", taskID, "expected", expected, "got", deviceID)
		return
	}

	res := TaskResult{
		TaskID:   taskID,
		DeviceID: deviceID,
		Attempt:  attempt,
		// 端侧结果一律不可信（不变量 4）：即使端侧声称 trusted 也强制覆盖
		SourceTrust: model.TrustUntrusted,
		Duration:    d.now().Sub(started),
	}
	switch p.Status {
	case TaskStatusSuccess:
		res.Status = model.ToolOK
		res.Result = p.Result
	case TaskStatusCancelled:
		res.Status = model.ToolError
		res.Error = "cancelled"
		if p.Error != "" {
			res.Error = "cancelled: " + p.Error
		}
	case TaskStatusFailed:
		res.Status = model.ToolError
		res.Error = p.Error
	default:
		res.Status = model.ToolError
		res.Error = TaskErrorMalformed
		if p.Error != "" {
			res.Error = TaskErrorMalformed + ": " + p.Error
		}
	}
	if res.Status == model.ToolError && res.Error == "" {
		res.Error = "端侧返回失败但未给出原因"
	}

	final := task.finish(res)
	if final.TaskID == "" {
		// 已被 timeout 抢先收敛：端侧结果丢弃，但仍记录日志便于排查
		d.log.Warn("任务已超时收敛，丢弃迟到的端侧结果",
			"task_id", taskID, "device_id", deviceID, "status", p.Status)
		return
	}
	d.record(final)
	d.log.Info("端侧任务已收敛",
		"task_id", taskID, "device_id", deviceID, "status", final.Status,
		"duration_ms", int(final.Duration/time.Millisecond), "trust", final.SourceTrust)
}

// FailDevice 设备离线时立即收敛其全部在途任务（离线立即降级，不等 deadline）。
//
// 返回受影响任务数。由 cmd 装配成 Hub.OnOffline 的组合回调调用。
func (d *Dispatcher) FailDevice(deviceID, reason string) int {
	if deviceID == "" {
		return 0
	}
	if reason == "" {
		reason = TaskErrorOffline
	}
	d.mu.Lock()
	var victims []*pendingTask
	for _, task := range d.pending {
		if task.deviceID == deviceID {
			victims = append(victims, task)
		}
	}
	d.mu.Unlock()

	for _, task := range victims {
		res := task.finish(TaskResult{
			TaskID:      task.taskID,
			DeviceID:    deviceID,
			Status:      model.ToolError,
			Error:       reason,
			SourceTrust: model.TrustUntrusted,
			Degraded:    true,
			Attempt:     task.attempt,
			Duration:    d.now().Sub(task.started),
		})
		if res.TaskID == "" {
			continue
		}
		d.record(res)
		d.log.Warn("设备离线，在途任务立即降级",
			"task_id", res.TaskID, "device_id", deviceID, "reason", reason)
	}
	return len(victims)
}

// PendingCount 待派发任务数（deviceID 为空时统计全部；填充 pong.pending）。
func (d *Dispatcher) PendingCount(deviceID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if deviceID == "" {
		return len(d.pending)
	}
	n := 0
	for _, task := range d.pending {
		if task.deviceID == deviceID {
			n++
		}
	}
	return n
}

// Close 关闭派发器：在途任务全部以失败收敛（服务端关闭，端侧不再回结果）。
func (d *Dispatcher) Close(reason string) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	tasks := make([]*pendingTask, 0, len(d.pending))
	for _, task := range d.pending {
		tasks = append(tasks, task)
	}
	d.mu.Unlock()

	if reason == "" {
		reason = FrameCodeServerClosing
	}
	for _, task := range tasks {
		task.finish(TaskResult{
			TaskID:      task.taskID,
			DeviceID:    task.deviceID,
			Status:      model.ToolError,
			Error:       reason,
			SourceTrust: model.TrustUntrusted,
			Degraded:    true,
			Attempt:     task.attempt,
		})
	}
}

// ============ 内部状态管理 ============

// register 登记在途任务（同一 task_id 重复登记 → ErrTaskInFlight）。
func (d *Dispatcher) register(task *pendingTask) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ErrDeviceUnavailable
	}
	if _, ok := d.pending[task.taskID]; ok {
		return ErrTaskInFlight
	}
	d.pending[task.taskID] = task
	return nil
}

// removePending 摘除在途任务（不影响已写入 ch 的终态）。
func (d *Dispatcher) removePending(taskID string) {
	d.mu.Lock()
	delete(d.pending, taskID)
	d.mu.Unlock()
}

// isClosed 是否已关闭。
func (d *Dispatcher) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// record 写入幂等缓存（带过期清理，容量上限按最旧淘汰）。
func (d *Dispatcher) record(res TaskResult) {
	if res.TaskID == "" {
		return
	}
	// 降级结果不入幂等缓存：端侧恢复后同一 task_id 必须可以重新派发
	if res.Degraded {
		return
	}
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, e := range d.history {
		if now.Sub(e.at) > dispatchHistoryTTL {
			delete(d.history, id)
		}
	}
	if len(d.history) >= dispatchHistoryLimit {
		oldestID := ""
		var oldest time.Time
		for id, e := range d.history {
			if oldestID == "" || e.at.Before(oldest) {
				oldestID, oldest = id, e.at
			}
		}
		if oldestID != "" {
			delete(d.history, oldestID)
		}
	}
	d.history[res.TaskID] = historyEntry{res: res, at: now}
}

// cached 命中幂等缓存（仅成功收敛的终态才缓存；降级结果允许重派）。
func (d *Dispatcher) cached(taskID string) (TaskResult, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.history[taskID]
	if !ok {
		return TaskResult{}, false
	}
	if d.now().Sub(e.at) > dispatchHistoryTTL {
		delete(d.history, taskID)
		return TaskResult{}, false
	}
	return e.res, true
}
