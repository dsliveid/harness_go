package service

// ChildRunner 实现 toolapi.ChildRunner：Host.Spawn 的内核侧（阶段 F）。
//
// 语义（docs/插件体系设计.md D13）：
//   - 子 Run 是**真实 Run**（落库、UI 可见、SSE 可订阅），与普通 Run 同一执行引擎；
//   - **禁递归**：深度上限（默认 2 = 父 + 子），子的子一律拒绝；
//   - 并发上限：同会话在途子 Run ≤ MaxConcurrent（默认 4）；
//   - 锁键：子 Run 用 `sessionID|spawn|<runID>`（engine.RunRequest.LockKey），
//     绕开父 Run 持有的会话锁——父 goroutine 在 Wait 阻塞、不并发追加消息，
//     seq 安全性由「父等待子」的串行性保证；
//   - Output = 子 Run 最后一条 assistant 消息（工具结果回灌父 Run 的模型）。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"harness-go/internal/agent"
	"harness-go/internal/approval"
	"harness-go/internal/model"
	"harness-go/internal/prompt"
	"harness-go/internal/store"
	"harness-go/pkg/toolapi"
)

// 默认参数。
const (
	// DefaultMaxSpawnDepth 嵌套深度上限（2 = 父 + 子；子的子禁止——D13 禁递归）。
	DefaultMaxSpawnDepth = 2
	// DefaultMaxConcurrentChildren 同会话在途子 Run 上限。
	DefaultMaxConcurrentChildren = 4
	// childRunTimeout 子 Run 单独超时（防父无限等待）。
	childRunTimeout = 10 * time.Minute
)

// ChildRunnerOptions 依赖。
type ChildRunnerOptions struct {
	Sessions *store.SessionStore
	Engine   *agent.Engine
	Main     *store.MainStore
	// ToolRefs 工具定义提供者（与 RunService 同一来源，保证子 Run 能调用同样工具）。
	ToolRefs   func() []prompt.ToolRef
	SystemBase string
	SOP        []string
	// SOPFunc 动态 SOP 源（阶段 G；非 nil 时优先于静态 SOP）。
	SOPFunc func() []string
	// MaxDepth 深度上限（<=0 用默认 2）。
	MaxDepth int
	// MaxConcurrent 同会话在途上限（<=0 用默认 4）。
	MaxConcurrent int
}

// ChildRunner 嵌套 Run 派生器。
type ChildRunner struct {
	opts ChildRunnerOptions
	// sopFunc 动态 SOP 源缓存（构造期固化，避免每轮读盘的锁竞争面）。
	sopFunc func() []string
	mu      sync.Mutex
	// parentOf childRunID → parentRunID（深度链）
	parentOf map[string]string
	// active 同会话在途计数
	active map[string]int
	// live 在途句柄（Get 语义：在途子 Run 可查）
	live map[string]*childRun
	// cancelFuncs 在途子 Run 的取消函数
	cancelFuncs map[string]context.CancelFunc
}

// NewChildRunner 创建派生器。
func NewChildRunner(opts ChildRunnerOptions) *ChildRunner {
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = DefaultMaxSpawnDepth
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = DefaultMaxConcurrentChildren
	}
	return &ChildRunner{
		opts:        opts,
		sopFunc:     opts.SOPFunc,
		parentOf:    make(map[string]string),
		active:      make(map[string]int),
		live:        make(map[string]*childRun),
		cancelFuncs: make(map[string]context.CancelFunc),
	}
}

// currentSOP 返回子 Run 的 SOP 注入（动态源优先）。
func (r *ChildRunner) currentSOP() []string {
	if r.sopFunc != nil {
		return r.sopFunc()
	}
	return r.opts.SOP
}

// childRun 子 Run 句柄。
type childRun struct {
	id      string
	session string
	r       *ChildRunner
	ch      chan struct{} // 关闭 = 终态
	result  toolapi.ChildResult
}

// ID 实现 toolapi.ChildRun。
func (c *childRun) ID() string { return c.id }

// Status 实现 toolapi.ChildRun（终态前返回 running；未落库的创建失败不可达此路径）。
func (c *childRun) Status() toolapi.ChildStatus {
	select {
	case <-c.ch:
		return c.result.Status
	default:
		return toolapi.ChildRunning
	}
}

// Wait 实现 toolapi.ChildRun（阻塞到终态）。
func (c *childRun) Wait(ctx context.Context) (toolapi.ChildResult, error) {
	select {
	case <-c.ch:
		return c.result, nil
	case <-ctx.Done():
		return toolapi.ChildResult{}, ctx.Err()
	}
}

// Cancel 实现 toolapi.ChildRun（尽力：标记取消由引擎 ctx 传播——
// 子 Run 的 ctx 由 Spawn 持有，Cancel 关闭其 context）。
func (c *childRun) Cancel() error {
	c.r.cancelChild(c.id)
	return nil
}

// Spawn 实现 toolapi.ChildRunner。
func (r *ChildRunner) Spawn(ctx context.Context, req toolapi.SpawnRequest) (toolapi.ChildRun, error) {
	if strings.TrimSpace(req.SessionID) == "" {
		return nil, fmt.Errorf("%w: SessionID 不能为空", toolapi.ErrInvalidArgs)
	}
	if strings.TrimSpace(req.Input) == "" {
		return nil, fmt.Errorf("%w: Input 不能为空", toolapi.ErrInvalidArgs)
	}
	r.mu.Lock()
	// ① 深度链检查：ParentRunID 的深度 = 其已登记祖先链长 + 1；
	// 新 Run 深度 = ParentRunID 深度 + 1 = 链长 + 2。≥ MaxDepth+1 即拒绝
	// （D13 禁递归：MaxDepth=2 时 父(1)→子(2) 合法，子派孙(3) 拒绝）。
	chainLen := 0
	cur := req.ParentRunID
	for cur != "" && chainLen <= r.opts.MaxDepth {
		parent, ok := r.parentOf[cur]
		if !ok {
			break
		}
		chainLen++
		cur = parent
	}
	if chainLen+2 > r.opts.MaxDepth {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: 嵌套深度超限（上限 %d），子 Run 禁止再派生（禁递归）",
			toolapi.ErrInvalidArgs, r.opts.MaxDepth)
	}
	// ② 并发上限
	if r.active[req.SessionID] >= r.opts.MaxConcurrent {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: 同会话在途子 Run 已达上限 %d", toolapi.ErrInvalidArgs, r.opts.MaxConcurrent)
	}
	r.mu.Unlock()

	sdb, err := r.opts.Sessions.Acquire(ctx, req.SessionID)
	if err != nil {
		return nil, fmt.Errorf("service: 打开会话库失败: %w", err)
	}
	// ③ 模型回退链：显式指定 > 会话配置 > default（与 RunService.CreateRunAs 同口径）
	modelRef := req.ModelRef
	if modelRef == "" {
		if sess, gerr := r.opts.Main.Sessions().Get(ctx, req.SessionID); gerr == nil && sess.ModelRef != "" {
			modelRef = sess.ModelRef
		} else {
			modelRef = "default"
		}
	}
	// ④ 创建真实 Run 行（UI 可见）
	run := &model.Run{
		ID:        newID(),
		Status:    model.RunPending,
		ModelRef:  modelRef,
		Executor:  model.ExecutorServer,
		CreatedAt: store.NowMillis(),
	}
	if err := sdb.Runs().Create(ctx, run); err != nil {
		return nil, fmt.Errorf("service: 创建子 Run 失败: %w", err)
	}
	input := req.Input
	if strings.TrimSpace(req.AgentRole) != "" {
		// 角色注入：协作者的视角约束进输入前缀（基础服务设计 §5 collaborator 语义）
		input = fmt.Sprintf("【角色：%s】%s", req.AgentRole, req.Input)
	}

	cr := &childRun{id: run.ID, session: req.SessionID, r: r, ch: make(chan struct{})}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), childRunTimeout)
	r.registerChild(run.ID, req.ParentRunID, req.SessionID, cancel, cr)

	// 审批口径与会话一致（子 Run 不因嵌套而放大权限）；锁键绕开父持有的会话锁。
	accessMode := approval.AccessMode(normalizeAccessMode(sessAccessMode(ctx, r.opts.Main, req.SessionID)))
	areq := agent.RunRequest{
		RunID:      run.ID,
		SessionID:  req.SessionID,
		UserInput:  input,
		ModelRef:   modelRef,
		AccessMode: accessMode,
		SystemBase: r.opts.SystemBase,
		SOP:        r.currentSOP(),
		LockKey:    spawnLockKey(req.SessionID, run.ID),
	}
	if r.opts.ToolRefs != nil {
		areq.Tools = r.opts.ToolRefs()
	}
	go func() {
		defer func() {
			r.unregisterChild(run.ID, req.SessionID)
			cancel()
			close(cr.ch) // 终态信号（result 已先写入）
		}()
		_ = r.opts.Engine.Execute(runCtx, areq)
		// 终态与 Output 以 DB 为准
		cr.result = r.collectResult(context.Background(), req.SessionID, run.ID)
	}()
	return cr, nil
}

// Get 实现 toolapi.ChildRunner（在途子 Run 句柄查询）。
func (r *ChildRunner) Get(runID string) (toolapi.ChildRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.live[runID]
	return c, ok
}

// ChildStatusOf 查询子 Run 终态（历史回放）。
func (r *ChildRunner) ChildStatusOf(ctx context.Context, sessionID, runID string) (toolapi.ChildStatus, error) {
	sdb, err := r.opts.Sessions.Acquire(ctx, sessionID)
	if err != nil {
		return "", err
	}
	run, err := sdb.Runs().Get(ctx, runID)
	if err != nil {
		return "", err
	}
	switch run.Status {
	case model.RunCompleted:
		return toolapi.ChildCompleted, nil
	case model.RunFailed:
		return toolapi.ChildFailed, nil
	case model.RunCancelled:
		return toolapi.ChildCancelled, nil
	case model.RunInterrupted:
		return toolapi.ChildInterrupted, nil
	case model.RunWaitingApproval:
		return toolapi.ChildWaitingApproval, nil
	default:
		return toolapi.ChildRunning, nil
	}
}

// collectResult 子 Run 终态收集：最后一条 assistant 消息为 Output。
func (r *ChildRunner) collectResult(ctx context.Context, sessionID, runID string) toolapi.ChildResult {
	res := toolapi.ChildResult{RunID: runID}
	sdb, err := r.opts.Sessions.Acquire(ctx, sessionID)
	if err != nil {
		res.Status, res.Error = toolapi.ChildFailed, err.Error()
		return res
	}
	run, err := sdb.Runs().Get(ctx, runID)
	if err != nil {
		res.Status, res.Error = toolapi.ChildFailed, err.Error()
		return res
	}
	switch run.Status {
	case model.RunCompleted:
		res.Status = toolapi.ChildCompleted
	case model.RunFailed:
		res.Status, res.Error = toolapi.ChildFailed, run.ErrorMessage
	case model.RunCancelled:
		res.Status = toolapi.ChildCancelled
	default:
		res.Status, res.Error = toolapi.ChildInterrupted, "Run 非终态: "+string(run.Status)
	}
	if msgs, lerr := sdb.Messages().List(ctx, 0, 1000); lerr == nil {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].RunID == runID && msgs[i].Role == model.RoleAssistant {
				res.Output = msgs[i].Content
				break
			}
		}
	}
	return res
}

func (r *ChildRunner) registerChild(runID, parentID, sessionID string, cancel context.CancelFunc, c *childRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if parentID != "" {
		r.parentOf[runID] = parentID
	}
	r.active[sessionID]++
	r.live[runID] = c
	r.cancelFuncs[runID] = cancel
}

func (r *ChildRunner) unregisterChild(runID, sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.parentOf, runID)
	if r.active[sessionID] > 0 {
		r.active[sessionID]--
		if r.active[sessionID] <= 0 {
			delete(r.active, sessionID)
		}
	}
	delete(r.live, runID)
	delete(r.cancelFuncs, runID)
}

func (r *ChildRunner) cancelChild(runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.cancelFuncs[runID]; ok {
		c()
	}
}

// sessAccessMode 读会话访问模式（默认 confirm）。
func sessAccessMode(ctx context.Context, main *store.MainStore, sessionID string) string {
	if main == nil {
		return string(approval.ModeConfirm)
	}
	sess, err := main.Sessions().Get(ctx, sessionID)
	if err != nil || sess == nil || sess.AccessMode == "" {
		return string(approval.ModeConfirm)
	}
	return sess.AccessMode
}

// spawnLockKey 子 Run 的会话锁键。
func spawnLockKey(sessionID, runID string) string {
	return sessionID + "|spawn|" + runID
}

// 编译期契约。
var _ toolapi.ChildRunner = (*ChildRunner)(nil)
