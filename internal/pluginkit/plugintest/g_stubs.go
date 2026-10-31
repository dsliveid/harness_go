package plugintest

// 阶段 G 测试桩：快照（tempdir CAS）/ 长任务（内存状态机）/ 导出包（记录桩）。
//
// 语义与内核实装一致（快照登记/恢复、任务状态迁移、检查点=Run+快照引用），
// 但不带 DB——插件测试只关注工具语义；内核侧 store/service 测试覆盖持久化。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"harness-go/pkg/toolapi"
)

// ---------- 快照桩 ----------

type snapStub struct {
	h     *Host
	mu    sync.Mutex
	blobs map[string][]byte        // hash → 内容
	snaps map[string]*snapStubItem // id → 快照
	seq   int
}

// snapBinding 单路径绑定（快照桩内部记录）。
type snapBinding struct {
	Path   string
	Hash   string
	Size   int64
	Absent bool
}

type snapStubItem struct {
	ref      toolapi.SnapshotRef
	bindings []snapBinding
}

func (h *Host) snapshotStub() toolapi.SnapshotStore {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.snap == nil {
		h.snap = &snapStub{h: h, blobs: map[string][]byte{}, snaps: map[string]*snapStubItem{}}
	}
	return h.snap
}

// SnapshotRefBindings 测试断言用：读取快照登记的路径绑定。
func (h *Host) SnapshotRefBindings(id string) []snapBinding {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.snap == nil {
		return nil
	}
	it, ok := h.snap.snaps[id]
	if !ok {
		return nil
	}
	return it.bindings
}

func (s *snapStub) Create(ctx context.Context, paths []string) (toolapi.SnapshotRef, error) {
	return s.CreateAnchored(ctx, paths, toolapi.SnapshotOpt{})
}

func (s *snapStub) CreateAnchored(ctx context.Context, paths []string, opt toolapi.SnapshotOpt) (toolapi.SnapshotRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bindings := make([]snapBinding, 0, len(paths))
	var total int64
	for _, p := range paths {
		abs, err := s.h.Resolve(p)
		if err != nil {
			return toolapi.SnapshotRef{}, err
		}
		content, rErr := os.ReadFile(abs)
		switch {
		case rErr == nil:
			sum := sha256.Sum256(content)
			hash := hex.EncodeToString(sum[:])
			s.blobs[hash] = content
			bindings = append(bindings, snapBinding{Path: filepath.ToSlash(filepath.Clean(p)), Hash: hash, Size: int64(len(content))})
			total += int64(len(content))
		case os.IsNotExist(rErr):
			bindings = append(bindings, snapBinding{Path: filepath.ToSlash(filepath.Clean(p)), Absent: true})
		default:
			return toolapi.SnapshotRef{}, rErr
		}
	}
	s.seq++
	ref := toolapi.SnapshotRef{
		ID:        fmt.Sprintf("snap-%04d", s.seq),
		Paths:     bindingPaths(bindings),
		Size:      total,
		CreatedAt: time.Now(),
	}
	s.snaps[ref.ID] = &snapStubItem{ref: ref, bindings: bindings}
	return ref, nil
}

func (s *snapStub) Restore(_ context.Context, ref toolapi.SnapshotRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.snaps[ref.ID]
	if !ok {
		return fmt.Errorf("快照 %s 不存在", ref.ID)
	}
	for _, b := range it.bindings {
		abs, rErr := s.h.Resolve(b.Path)
		if rErr != nil {
			return rErr
		}
		if b.Absent {
			_ = os.Remove(abs)
			continue
		}
		content, ok := s.blobs[b.Hash]
		if !ok {
			return fmt.Errorf("快照内容 %s 缺失", b.Hash)
		}
		if wErr := os.MkdirAll(filepath.Dir(abs), 0o755); wErr != nil {
			return wErr
		}
		if wErr := os.WriteFile(abs, content, 0o644); wErr != nil {
			return wErr
		}
	}
	return nil
}

func (s *snapStub) List(context.Context) ([]toolapi.SnapshotRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]toolapi.SnapshotRef, 0, len(s.snaps))
	for _, it := range s.snaps {
		out = append(out, it.ref)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// ---------- 长任务桩 ----------

type taskStub struct {
	h      *Host
	mu     sync.Mutex
	seq    int
	tasks  map[string]*taskStubItem
	orders []string
}

type taskStubItem struct {
	spec  toolapi.TaskSpec
	state toolapi.TaskState
	cps   []toolapi.TaskCheckpoint
	snap  toolapi.SnapshotStore
	cpSeq int
}

func terminalState(s toolapi.TaskState) bool {
	return s == toolapi.TaskCompleted || s == toolapi.TaskFailed || s == toolapi.TaskCancelled
}

func (h *Host) taskStub() toolapi.TaskManager {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.task == nil {
		h.task = &taskStub{h: h, tasks: map[string]*taskStubItem{}}
	}
	return h.task
}

// TaskStubCheckpoints 测试断言用：读取任务的检查点。
func (h *Host) TaskStubCheckpoints(taskID string) []toolapi.TaskCheckpoint {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.task == nil {
		return nil
	}
	it, ok := h.task.tasks[taskID]
	if !ok {
		return nil
	}
	return append([]toolapi.TaskCheckpoint(nil), it.cps...)
}

func (m *taskStub) Start(_ context.Context, spec toolapi.TaskSpec) (toolapi.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.TrimSpace(spec.Title) == "" {
		return nil, fmt.Errorf("%w: title 不能为空", toolapi.ErrInvalidArgs)
	}
	m.seq++
	id := spec.ID
	if id == "" {
		id = fmt.Sprintf("task-%04d", m.seq)
	}
	if _, dup := m.tasks[id]; dup {
		return nil, fmt.Errorf("%w: 任务 %s 已存在", toolapi.ErrDuplicate, id)
	}
	spec.ID = id
	it := &taskStubItem{spec: spec, state: toolapi.TaskRunning, snap: m.h.snapshotStub()}
	m.tasks[id] = it
	m.orders = append(m.orders, id)
	return &taskStubHandle{m: m, id: id}, nil
}

func (m *taskStub) Get(id string) (toolapi.Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[id]; !ok {
		return nil, false
	}
	return &taskStubHandle{m: m, id: id}, true
}

func (m *taskStub) List() []toolapi.Task { return nil }

func (m *taskStub) Checkpoint(ctx context.Context, id string) (toolapi.TaskCheckpoint, error) {
	return m.CheckpointNote(ctx, id, "")
}

func (m *taskStub) CheckpointNote(_ context.Context, id, note string) (toolapi.TaskCheckpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.tasks[id]
	if !ok {
		return toolapi.TaskCheckpoint{}, fmt.Errorf("任务 %s 不存在", id)
	}
	paths := specPaths(it.spec)
	if len(paths) == 0 {
		return toolapi.TaskCheckpoint{}, fmt.Errorf("%w: 任务未声明 paths，无法检查点", toolapi.ErrInvalidArgs)
	}
	it.cpSeq++
	reg, ok := it.snap.(toolapi.SnapshotRegistrar)
	if !ok {
		return toolapi.TaskCheckpoint{}, fmt.Errorf("快照桩不支持锚点登记")
	}
	ref, err := reg.CreateAnchored(context.Background(), paths, toolapi.SnapshotOpt{Kind: "checkpoint", TaskID: id})
	if err != nil {
		return toolapi.TaskCheckpoint{}, err
	}
	cp := toolapi.TaskCheckpoint{
		TaskID:     id,
		RunID:      it.spec.RunID,
		SnapshotID: ref.ID,
		Note:       note,
		CreatedAt:  time.Now(),
	}
	it.cps = append(it.cps, cp)
	return cp, nil
}

func (m *taskStub) ListSession(_ context.Context, sessionID string) ([]toolapi.TaskSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []toolapi.TaskSummary{}
	for _, id := range m.orders {
		it := m.tasks[id]
		if it.spec.SessionID != sessionID {
			continue
		}
		out = append(out, toolapi.TaskSummary{
			ID:        id,
			Title:     it.spec.Title,
			State:     it.state,
			RunID:     it.spec.RunID,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		})
	}
	return out, nil
}

func (m *taskStub) Checkpoints(_ context.Context, taskID string) ([]toolapi.TaskCheckpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.tasks[taskID]
	if !ok {
		return nil, fmt.Errorf("任务 %s 不存在", taskID)
	}
	return append([]toolapi.TaskCheckpoint(nil), it.cps...), nil
}

// Finish 实现 toolapi.TaskFinisher（completed / failed）。
func (m *taskStub) Finish(_ context.Context, id string, to toolapi.TaskState, _ string) error {
	if to != toolapi.TaskCompleted && to != toolapi.TaskFailed {
		return fmt.Errorf("Finish 只接受 completed/failed，得到 %s", to)
	}
	return m.transition(id, to)
}

type taskStubHandle struct {
	m  *taskStub
	id string
}

func (t *taskStubHandle) ID() string { return t.id }

func (t *taskStubHandle) State() toolapi.TaskState {
	t.m.mu.Lock()
	defer t.m.mu.Unlock()
	return t.m.tasks[t.id].state
}

func (t *taskStubHandle) Steps() []toolapi.TaskStep {
	t.m.mu.Lock()
	defer t.m.mu.Unlock()
	return append([]toolapi.TaskStep(nil), t.m.tasks[t.id].spec.Steps...)
}

func (t *taskStubHandle) Pause() error  { return t.m.transition(t.id, toolapi.TaskPaused) }
func (t *taskStubHandle) Resume() error { return t.m.transition(t.id, toolapi.TaskRunning) }
func (t *taskStubHandle) Cancel() error { return t.m.transition(t.id, toolapi.TaskCancelled) }

func (m *taskStub) transition(id string, to toolapi.TaskState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("任务 %s 不存在", id)
	}
	switch to {
	case toolapi.TaskRunning:
		if it.state == toolapi.TaskCancelled || it.state == toolapi.TaskFailed || it.state == toolapi.TaskCompleted {
			return fmt.Errorf("任务已终态 %s，不可 resume", it.state)
		}
	case toolapi.TaskPaused:
		if it.state != toolapi.TaskRunning {
			return fmt.Errorf("仅 running 可暂停，当前 %s", it.state)
		}
	case toolapi.TaskCancelled:
		switch it.state {
		case toolapi.TaskCompleted, toolapi.TaskFailed, toolapi.TaskCancelled:
			return fmt.Errorf("任务已终态 %s", it.state)
		}
	}
	it.state = to
	return nil
}

// specPaths 从 TaskSpec.Meta 取 paths 声明。
func specPaths(spec toolapi.TaskSpec) []string {
	raw, ok := spec.Meta["paths"]
	if !ok {
		return nil
	}
	list, ok := raw.([]string)
	if ok {
		return list
	}
	if anys, ok := raw.([]any); ok {
		out := make([]string, 0, len(anys))
		for _, a := range anys {
			if s, ok := a.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// ---------- 导出包桩 ----------

type bundleStub struct {
	mu      sync.Mutex
	exports []toolapi.BundleInfo
	imports []string
}

func (h *Host) bundleStub() toolapi.BundleStore {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.bundle == nil {
		h.bundle = &bundleStub{}
	}
	return h.bundle
}

// BundleStubExports 测试断言用。
func (h *Host) BundleStubExports() []toolapi.BundleInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]toolapi.BundleInfo(nil), h.bundle.exports...)
}

func (b *bundleStub) Export(_ context.Context, sessionID string) (toolapi.BundleInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	info := toolapi.BundleInfo{SessionID: sessionID, Path: "stub://" + sessionID, CreatedAt: time.Now()}
	b.exports = append(b.exports, info)
	return info, nil
}

func (b *bundleStub) Import(_ context.Context, zipPath string) (toolapi.BundleInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.imports = append(b.imports, zipPath)
	return toolapi.BundleInfo{SessionID: "imported-stub", Path: zipPath, CreatedAt: time.Now()}, nil
}

func (b *bundleStub) List(_ context.Context, sessionID string) ([]toolapi.BundleInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []toolapi.BundleInfo
	for _, e := range b.exports {
		if e.SessionID == sessionID {
			out = append(out, e)
		}
	}
	return out, nil
}

func bindingPaths(bs []snapBinding) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Path)
	}
	return out
}
