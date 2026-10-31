// TaskManager 实现 toolapi.TaskManager：长任务状态机 + 检查点（阶段 G / D13）。
//
// 语义（docs/插件体系设计.md §3.4「基础服务保留协议、工具只做入口」）：
//   - 任务是**真实持久态**（会话库 tasks/task_checkpoints 表），重启后可查询；
//   - 检查点 = Run + 快照引用的**原子登记**：先落快照（全局 CAS + 主库行），
//     再落检查点行；后者失败补偿删除快照行（跨库无事务，与 blob 先文件后库同序）；
//   - Cancel 联动取消关联 Run（可选注入 RunStop）——真正的控制而非仅改标记；
//   - Pause/Resume 是状态机迁移（模型在下一轮通过查询感知并调整行为），
//     不劫持正在执行的 Run。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/model"
	"harness-go/internal/store"
	"harness-go/pkg/toolapi"
)

// TaskManagerOptions 长任务状态机依赖。
type TaskManagerOptions struct {
	// Sessions 会话库路由（tasks / task_checkpoints 表）。
	Sessions *store.SessionStore
	// Snapshots 检查点快照源（须实现 toolapi.SnapshotRegistrar）。
	Snapshots toolapi.SnapshotStore
	// RunStop 可选：取消任务关联的 Run（Cancel 联动；nil = 只迁移任务状态）。
	RunStop func(ctx context.Context, sessionID, runID string) error
	// Now 时钟（测试注入）。
	Now func() time.Time
}

// TaskManager 长任务状态机。
type TaskManager struct {
	opts TaskManagerOptions
	mu   sync.Mutex
	live map[string]*liveTask
}

// liveTask 在途任务的内存态（持久态以会话库为准）。
type liveTask struct {
	id        string
	sessionID string
	title     string
	runID     string
	state     model.TaskState
	steps     []model.TaskStep
	meta      map[string]any
}

// NewTaskManager 创建长任务状态机。
func NewTaskManager(opts TaskManagerOptions) *TaskManager {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	opts.Now = now
	return &TaskManager{opts: opts, live: make(map[string]*liveTask)}
}

// 编译期断言：冻结契约 + 可选扩展全量实装。
var (
	_ toolapi.TaskManager          = (*TaskManager)(nil)
	_ toolapi.TaskNoteCheckpointer = (*TaskManager)(nil)
	_ toolapi.TaskLister           = (*TaskManager)(nil)
	_ toolapi.TaskCheckpointReader = (*TaskManager)(nil)
	_ toolapi.TaskFinisher         = (*TaskManager)(nil)
)

// Start 实现 toolapi.TaskManager：登记任务（持久化）+ 建立在途句柄。
func (m *TaskManager) Start(ctx context.Context, spec toolapi.TaskSpec) (toolapi.Task, error) {
	title := strings.TrimSpace(spec.Title)
	if title == "" {
		return nil, fmt.Errorf("%w: title 不能为空", toolapi.ErrInvalidArgs)
	}
	if strings.TrimSpace(spec.SessionID) == "" {
		return nil, fmt.Errorf("%w: SessionID 不能为空", toolapi.ErrInvalidArgs)
	}
	id := strings.TrimSpace(spec.ID)
	if id == "" {
		id = newTaskID()
	}
	meta := spec.Meta
	if meta == nil {
		meta = map[string]any{}
	}
	t := &liveTask{
		id:        id,
		sessionID: spec.SessionID,
		title:     title,
		runID:     spec.RunID,
		state:     model.TaskRunning,
		steps:     toModelSteps(spec.Steps),
		meta:      meta,
	}
	if err := m.persist(ctx, t); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.live[id] = t
	m.mu.Unlock()
	return &taskHandle{m: m, id: id}, nil
}

// Get 实现 toolapi.TaskManager（在途优先，退回持久态）。
func (m *TaskManager) Get(id string) (toolapi.Task, bool) {
	m.mu.Lock()
	_, ok := m.live[id]
	m.mu.Unlock()
	if ok {
		return &taskHandle{m: m, id: id}, true
	}
	// 持久态存在但不在途（重启后）：仍可查询与暂停/取消（下次迁移落库）
	return nil, false
}

// GetPersisted 读取持久态任务（含重启后；ListSession 同源）。
func (m *TaskManager) GetPersisted(ctx context.Context, sessionID, id string) (model.Task, error) {
	sdb, err := m.opts.Sessions.Acquire(ctx, sessionID)
	if err != nil {
		return model.Task{}, err
	}
	row, err := sdb.Tasks().Get(ctx, id)
	if err != nil {
		return model.Task{}, wrapStoreErr(err, apperr.CodeNotFound, "长任务不存在")
	}
	return row, nil
}

// List 实现 toolapi.TaskManager：在途任务。
func (m *TaskManager) List() []toolapi.Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]toolapi.Task, 0, len(m.live))
	for id := range m.live {
		out = append(out, &taskHandle{m: m, id: id})
	}
	return out
}

// ListSession 实现 toolapi.TaskLister：会话内任务（内存态覆盖持久态）。
func (m *TaskManager) ListSession(ctx context.Context, sessionID string) ([]toolapi.TaskSummary, error) {
	sdb, err := m.opts.Sessions.Acquire(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	rows, err := sdb.Tasks().List(ctx)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeInternal, "读取任务失败")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]toolapi.TaskSummary, 0, len(rows))
	for _, row := range rows {
		state := row.State
		if t, ok := m.live[row.ID]; ok {
			state = t.state
		}
		out = append(out, toolapi.TaskSummary{
			ID:        row.ID,
			Title:     row.Title,
			State:     toolapi.TaskState(state),
			RunID:     row.RunID,
			CreatedAt: time.UnixMilli(row.CreatedAt),
			UpdatedAt: time.UnixMilli(row.UpdatedAt),
		})
	}
	return out, nil
}

// Checkpoint 实现 toolapi.TaskManager（无备注）。
func (m *TaskManager) Checkpoint(ctx context.Context, id string) (toolapi.TaskCheckpoint, error) {
	return m.CheckpointNote(ctx, id, "")
}

// CheckpointNote 实现 toolapi.TaskNoteCheckpointer：检查点 = Run + 快照引用。
func (m *TaskManager) CheckpointNote(ctx context.Context, id, note string) (toolapi.TaskCheckpoint, error) {
	t := m.snapshotLive(id)
	if t == nil {
		return toolapi.TaskCheckpoint{}, fmt.Errorf("%w: 长任务 %s 不在途（重启后任务不可再检查点，ADR-009）", toolapi.ErrInvalidArgs, id)
	}
	reg, ok := m.opts.Snapshots.(toolapi.SnapshotRegistrar)
	if !ok {
		return toolapi.TaskCheckpoint{}, fmt.Errorf("%w: 快照存储不支持锚点登记（内核未接线 SnapshotRegistrar）", toolapi.ErrNotImplemented)
	}
	paths := stringSlice(t.meta["paths"])
	if len(paths) == 0 {
		return toolapi.TaskCheckpoint{}, fmt.Errorf("%w: 任务未声明 paths，无法生成检查点快照（start_long_task 时声明 paths）", toolapi.ErrInvalidArgs)
	}
	ref, err := reg.CreateAnchored(ctx, paths, toolapi.SnapshotOpt{Kind: model.SnapshotKindCheckpoint, TaskID: id})
	if err != nil {
		return toolapi.TaskCheckpoint{}, fmt.Errorf("生成检查点快照失败: %w", err)
	}
	cp := model.TaskCheckpoint{
		ID:         newCheckpointID(),
		TaskID:     id,
		RunID:      t.runID,
		SnapshotID: ref.ID,
		Note:       strings.TrimSpace(note),
		CreatedAt:  m.opts.Now().UnixMilli(),
	}
	sdb, err := m.opts.Sessions.Acquire(ctx, t.sessionID)
	if err != nil {
		_ = m.deleteSnapshot(ctx, ref.ID) // 补偿：检查点行落不了，快照不留孤儿
		return toolapi.TaskCheckpoint{}, err
	}
	if err := sdb.TaskCheckpoints().Add(ctx, &cp); err != nil {
		_ = m.deleteSnapshot(ctx, ref.ID) // 补偿（先文件后库的逆操作）
		return toolapi.TaskCheckpoint{}, wrapStoreErr(err, apperr.CodeInternal, "登记检查点失败")
	}
	return toolapi.TaskCheckpoint{
		TaskID:     cp.TaskID,
		RunID:      cp.RunID,
		SnapshotID: cp.SnapshotID,
		Note:       cp.Note,
		CreatedAt:  time.UnixMilli(cp.CreatedAt),
	}, nil
}

// Checkpoints 实现 toolapi.TaskCheckpointReader（在途任务的会话由 live 表定位）。
func (m *TaskManager) Checkpoints(ctx context.Context, taskID string) ([]toolapi.TaskCheckpoint, error) {
	m.mu.Lock()
	t := m.live[taskID]
	m.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("%w: 长任务 %s 不在途（历史检查点走 snapshots API）", toolapi.ErrInvalidArgs, taskID)
	}
	return m.CheckpointsInSession(ctx, t.sessionID, taskID)
}

// CheckpointsInSession 读取检查点（含重启后的持久态；sessionID 显式给出）。
func (m *TaskManager) CheckpointsInSession(ctx context.Context, sessionID, taskID string) ([]toolapi.TaskCheckpoint, error) {
	sdb, err := m.opts.Sessions.Acquire(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	rows, err := sdb.TaskCheckpoints().ListByTask(ctx, taskID)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeInternal, "读取检查点失败")
	}
	out := make([]toolapi.TaskCheckpoint, 0, len(rows))
	for _, cp := range rows {
		out = append(out, toolapi.TaskCheckpoint{
			TaskID:     cp.TaskID,
			RunID:      cp.RunID,
			SnapshotID: cp.SnapshotID,
			Note:       cp.Note,
			CreatedAt:  time.UnixMilli(cp.CreatedAt),
		})
	}
	return out, nil
}

// Finish 实现 toolapi.TaskFinisher（completed / failed）。
func (m *TaskManager) Finish(ctx context.Context, id string, to toolapi.TaskState, note string) error {
	if to != toolapi.TaskCompleted && to != toolapi.TaskFailed {
		return fmt.Errorf("%w: Finish 只接受 completed/failed，得到 %s", toolapi.ErrInvalidArgs, to)
	}
	return m.transition(ctx, id, model.TaskState(to), note)
}

// —— 内部 ——

// transition 迁移状态（含合法性校验）并持久化。
func (m *TaskManager) transition(ctx context.Context, id string, to model.TaskState, note string) error {
	m.mu.Lock()
	t := m.live[id]
	if t == nil {
		m.mu.Unlock()
		return fmt.Errorf("%w: 长任务 %s 不在途", toolapi.ErrInvalidArgs, id)
	}
	if err := validateTaskTransition(t.state, to); err != nil {
		m.mu.Unlock()
		return err
	}
	t.state = to
	m.mu.Unlock()
	if err := m.persistWithState(ctx, t, to); err != nil {
		return err
	}
	_ = note // 备注仅进检查点；状态迁移不携带（协议冻结面不加形参）
	return nil
}

// validateTaskTransition 状态机合法性（终态不可迁出；paused 只能从 running 进）。
func validateTaskTransition(from, to model.TaskState) error {
	if from.Terminal() {
		return fmt.Errorf("%w: 任务已终态 %s，不可再迁移到 %s", toolapi.ErrInvalidArgs, from, to)
	}
	if to == model.TaskPaused && from != model.TaskRunning {
		return fmt.Errorf("%w: 仅 running 可暂停，当前 %s", toolapi.ErrInvalidArgs, from)
	}
	return nil
}

// CancelTask 取消任务并联动取消关联 Run（toolapi.Task.Cancel 的内核路径）。
func (m *TaskManager) CancelTask(ctx context.Context, id string) error {
	m.mu.Lock()
	t := m.live[id]
	m.mu.Unlock()
	if t == nil {
		return fmt.Errorf("%w: 长任务 %s 不在途", toolapi.ErrInvalidArgs, id)
	}
	wasRunning := t.runID != ""
	if err := m.transition(ctx, id, model.TaskCancelled, ""); err != nil {
		return err
	}
	if wasRunning && m.opts.RunStop != nil {
		if err := m.opts.RunStop(ctx, t.sessionID, t.runID); err != nil {
			// 任务已取消成功；Run 取消失败不回滚任务状态（显式返回错误可见）
			return fmt.Errorf("任务已取消，但关联 Run 取消失败: %w", err)
		}
	}
	return nil
}

// persist 全量持久化（Start / 迁移共用）。
func (m *TaskManager) persist(ctx context.Context, t *liveTask) error {
	return m.persistWithState(ctx, t, t.state)
}

func (m *TaskManager) persistWithState(ctx context.Context, t *liveTask, state model.TaskState) error {
	sdb, err := m.opts.Sessions.Acquire(ctx, t.sessionID)
	if err != nil {
		return err
	}
	metaJSON, err := json.Marshal(t.meta)
	if err != nil {
		return fmt.Errorf("序列化任务 meta 失败: %w", err)
	}
	row := &model.Task{
		ID:        t.id,
		RunID:     t.runID,
		Title:     t.title,
		State:     state,
		Steps:     t.steps,
		MetaJSON:  metaJSON,
		CreatedAt: m.opts.Now().UnixMilli(),
	}
	if err := sdb.Tasks().Upsert(ctx, row); err != nil {
		return wrapStoreErr(err, apperr.CodeInternal, "持久化任务失败")
	}
	return nil
}

func (m *TaskManager) snapshotLive(id string) *liveTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.live[id]
	return t
}

func (m *TaskManager) deleteSnapshot(ctx context.Context, snapshotID string) error {
	type snapshotDeleter interface {
		Delete(ctx context.Context, id string) ([]string, error)
	}
	if d, ok := m.opts.Snapshots.(snapshotDeleter); ok {
		_, _ = d.Delete(ctx, snapshotID)
	}
	return nil
}

// taskHandle toolapi.Task 句柄（State 实时读内存态）。
type taskHandle struct {
	m  *TaskManager
	id string
}

func (h *taskHandle) ID() string { return h.id }

func (h *taskHandle) State() toolapi.TaskState {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if t := h.m.live[h.id]; t != nil {
		return toolapi.TaskState(t.state)
	}
	return ""
}

func (h *taskHandle) Steps() []toolapi.TaskStep {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	t := h.m.live[h.id]
	if t == nil {
		return nil
	}
	out := make([]toolapi.TaskStep, 0, len(t.steps))
	for _, s := range t.steps {
		out = append(out, toolapi.TaskStep{ID: s.ID, Title: s.Title, Status: toolapi.TaskState(s.Status), Note: s.Note})
	}
	return out
}

func (h *taskHandle) Pause() error {
	ctx := context.Background()
	return h.m.transition(ctx, h.id, model.TaskPaused, "")
}

func (h *taskHandle) Resume() error {
	ctx := context.Background()
	return h.m.transition(ctx, h.id, model.TaskRunning, "")
}

func (h *taskHandle) Cancel() error {
	return h.m.CancelTask(context.Background(), h.id)
}

// —— 小工具 ——

func toModelSteps(steps []toolapi.TaskStep) []model.TaskStep {
	out := make([]model.TaskStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, model.TaskStep{ID: s.ID, Title: s.Title, Status: model.TaskState(s.Status), Note: s.Note})
	}
	return out
}

func stringSlice(v any) []string {
	switch raw := v.(type) {
	case []string:
		return raw
	case []any:
		out := make([]string, 0, len(raw))
		for _, a := range raw {
			if s, ok := a.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func newTaskID() string       { return "task-" + newID() }
func newCheckpointID() string { return "cp-" + newID() }
