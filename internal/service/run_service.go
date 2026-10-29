package service

import (
	"context"
	"errors"
	"sync"

	"harness-go/internal/agent"
	"harness-go/internal/apperr"
	"harness-go/internal/approval"
	"harness-go/internal/model"
	"harness-go/internal/prompt"
	"harness-go/internal/store"
)

// ErrRunNotRunning 停止一个不在运行的 Run。
var ErrRunNotRunning = errors.New("service: Run 未在运行")

// RunServiceOptions Run 用例依赖。
type RunServiceOptions struct {
	Main       *store.MainStore
	Sessions   *store.SessionStore
	Engine     *agent.Engine
	SystemBase string
	SOP        []string
	Tools      []prompt.ToolRef
}

// RunService Run 用例：创建（异步执行）/ 停止 / 查询 / 启动恢复。
type RunService struct {
	main       *store.MainStore
	sessions   *store.SessionStore
	engine     *agent.Engine
	systemBase string
	sop        []string
	tools      []prompt.ToolRef

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// NewRunService 创建 Run 用例。
func NewRunService(opts RunServiceOptions) *RunService {
	return &RunService{
		main:       opts.Main,
		sessions:   opts.Sessions,
		engine:     opts.Engine,
		systemBase: opts.SystemBase,
		sop:        opts.SOP,
		tools:      opts.Tools,
		cancels:    make(map[string]context.CancelFunc),
	}
}

// CreateRun 写入用户消息 + 创建 Run(pending) + 异步执行。
//
// 返回后 Run 可能仍在 pending/running —— 调用方拿 run_id 去订阅 SSE。
func (s *RunService) CreateRun(ctx context.Context, sessionID, userInput, modelRef string) (*model.Run, error) {
	if userInput == "" {
		return nil, invalidArg("用户输入不能为空")
	}

	// 校验会话存在（只读主库）
	sess, err := s.main.Sessions().Get(ctx, sessionID)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeSessionNotFound, "会话不存在")
	}
	if modelRef == "" {
		modelRef = sess.ModelRef
	}
	if modelRef == "" {
		return nil, invalidArg("会话未配置模型，请先设置 model_ref")
	}

	sdb, err := s.sessions.Acquire(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	// ① 用户消息落库（会话库）
	userMsg := &model.Message{
		ID:        newID(),
		Role:      model.RoleUser,
		Content:   userInput,
		CreatedAt: store.NowMillis(),
	}
	if err := sdb.Messages().Append(ctx, userMsg); err != nil {
		return nil, err
	}
	// ② 摘要回写主库（最终一致）
	_ = s.main.Sessions().TouchMessage(ctx, sessionID, userMsg.CreatedAt)

	// ③ 创建 Run（pending）
	run := &model.Run{
		ID:        newID(),
		Status:    model.RunPending,
		ModelRef:  modelRef,
		Executor:  model.ExecutorServer,
		CreatedAt: store.NowMillis(),
	}
	if err := sdb.Runs().Create(ctx, run); err != nil {
		return nil, err
	}

	// ④ 异步执行：脱离 HTTP 请求 ctx（否则请求一结束 Run 就被取消）
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.registerCancel(run.ID, cancel)

	req := agent.RunRequest{
		RunID:          run.ID,
		SessionID:      sessionID,
		UserInput:      userInput,
		ModelRef:       modelRef,
		AccessMode:     approval.AccessMode(normalizeAccessMode(sess.AccessMode)),
		SystemBase:     s.systemBase,
		SOP:            s.sop,
		ProjectSummary: "",
		Tools:          s.tools,
	}
	go func() {
		defer func() {
			s.unregisterCancel(run.ID)
			cancel()
		}()
		// 执行结果以数据库为准，这里只记录日志（由上层 logger 接管前先忽略）
		_ = s.engine.Execute(runCtx, req)
	}()

	return run, nil
}

// StopRun 停止正在运行的 Run（ctx 取消 → 引擎迁移到 cancelled）。
func (s *RunService) StopRun(_ context.Context, runID string) error {
	s.mu.Lock()
	cancel, ok := s.cancels[runID]
	s.mu.Unlock()
	if !ok {
		return apperr.New(apperr.CodeRunStateConflict, "Run 未在运行")
	}
	cancel()
	return nil
}

// GetRun 查询 Run 状态。
func (s *RunService) GetRun(ctx context.Context, sessionID, runID string) (*model.Run, error) {
	sdb, err := s.sessions.Acquire(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	run, err := sdb.Runs().Get(ctx, runID)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeRunNotFound, "Run 不存在")
	}
	return run, nil
}

// ToolEvents 查询某 Run 的工具事件（UI 回放）。
func (s *RunService) ToolEvents(ctx context.Context, sessionID, runID string) ([]model.ToolEvent, error) {
	sdb, err := s.sessions.Acquire(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return sdb.ToolEvents().ListByRun(ctx, runID)
}

// RunningCount 当前在运行的 Run 数（维护接口用）。
func (s *RunService) RunningCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cancels)
}

// RecoverInterrupted 启动恢复：把未完成 Run 置 interrupted（ADR-009 不做断点续跑）。
//
// 只扫主库里 last_run_status 非终态的会话（**不全量扫会话库**）。
func (s *RunService) RecoverInterrupted(ctx context.Context) (int, error) {
	const pageSize = 200
	sessions, err := s.main.Sessions().List(ctx, pageSize, 0)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, sess := range sessions {
		if !sess.LastRunStatus.IsUnfinished() {
			continue
		}
		sdb, err := s.sessions.Acquire(ctx, sess.ID)
		if err != nil {
			continue // 单个会话库打不开不应阻塞启动
		}
		n, err := sdb.Runs().FailOpenRuns(ctx, "process_restart")
		if err != nil {
			continue
		}
		total += n
	}
	return total, nil
}

func (s *RunService) registerCancel(runID string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancels[runID] = cancel
}

func (s *RunService) unregisterCancel(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cancels, runID)
}

func normalizeAccessMode(mode string) string {
	switch mode {
	case string(approval.ModeFull):
		return string(approval.ModeFull)
	default:
		return string(approval.ModeConfirm)
	}
}
