package main

// 需求 §10-4/§10-5 验收（阶段 F 出口）：
//
//   - §10-4：Agent 能写文件、**写入触发审批**、可拒绝并**把拒绝原因回传模型**
//     （模型换方案继续，Run 不中断）；
//   - §10-5：审批规则 / 放行语义按访问模式与作用域生效。
//
// 全链路（装配点视角）：fake provider 发起 write_file → 真实 fs 插件注册表路由 →
// 审批闸门 → 拒绝 → 原因作为 tool 消息回灌 → 第二轮替代方案 → Run 完成。

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"harness-go/internal/model"

	"harness-go/internal/agent"
	"harness-go/internal/approval"
	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/plugins/fs"
	"harness-go/internal/prompt"
	"harness-go/internal/service"
	"harness-go/internal/snapshot"
	"harness-go/internal/store"
	"harness-go/internal/tokenizer"
	"harness-go/internal/tools"
	"harness-go/internal/tools/hostimpl"
	"harness-go/internal/tools/loader"
	"harness-go/internal/tools/registry"
	"harness-go/internal/tools/router"
	"harness-go/pkg/toolapi"
)

// presetProvider 预设轮次 provider（每轮依次输出；支持工具调用帧）。
type presetProvider struct {
	rounds [][]llm.Chunk
	idx    int
}

func (p *presetProvider) Name() string { return "preset" }

func (p *presetProvider) ChatStream(_ context.Context, _ llm.ChatRequest) (llm.Stream, error) {
	if p.idx >= len(p.rounds) {
		return nil, io.EOF
	}
	chunks := p.rounds[p.idx]
	p.idx++
	return &presetStream{chunks: chunks}, nil
}

type presetStream struct {
	chunks []llm.Chunk
	i      int
}

func (s *presetStream) Recv() (llm.Chunk, error) {
	if s.i >= len(s.chunks) {
		return llm.Chunk{}, io.EOF
	}
	c := s.chunks[s.i]
	s.i++
	return c, nil
}

func (s *presetStream) Close() error { return nil }

// fsApprovalEnv §10-4/5 测试环境（真实 fs 插件 + 审批闸门 + 沙箱工作区 + 快照存储）。
type fsApprovalEnv struct {
	engine    *agent.Engine
	main      *store.MainStore
	sessions  *store.SessionStore
	toolHost  *hostimpl.Host
	provider  *presetProvider
	toolReg   *registry.Registry
	sessionID string
	gate      *approval.Gate
	snaps     *snapshot.Store
	dataDir   string
}

func newFSApprovalEnv(t *testing.T, gateTimeout time.Duration, rounds [][]llm.Chunk) *fsApprovalEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	main, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = main.Close() })
	sessions, err := store.OpenSessionStore(store.SessionStoreOptions{DataDir: dir, MaxOpen: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessions.Close() })
	bus := events.NewBus(100, 32)
	t.Cleanup(bus.Close)

	toolReg := registry.New(registry.Options{CoreSoftLimit: 8, DeferredSoftLimit: 60})
	toolHost := hostimpl.New(hostimpl.Options{WorkDir: dir})
	// G：fs 写入强制「先快照后落盘」——测试环境与生产装配同口径接真实快照存储。
	snaps, err := snapshot.New(snapshot.Options{Main: main, DataDir: dir, WorkspaceRoot: dir})
	if err != nil {
		t.Fatalf("快照存储初始化失败: %v", err)
	}
	toolHost.SetSnapshots(snaps)
	if err := toolReg.RegisterFactory(ctx, fs.Factory(), toolHost, nil); err != nil {
		t.Fatalf("fs 插件装配失败: %v", err)
	}
	toolRunner := tools.NewRunner(toolReg, router.New(router.Options{
		Executor: toolapi.ExecutorServer,
		Capabilities: map[string]bool{
			toolapi.CapWorkspaceRead:  true,
			toolapi.CapWorkspaceWrite: true,
		},
	}), loader.Limits{CoreSoft: 8, DeferredSoft: 60})

	provider := &presetProvider{rounds: rounds}
	gate := approval.NewGate(gateTimeout)
	engine, err := agent.New(agent.Deps{
		Sessions: service.NewSessionProvider(sessions),
		Summary:  service.NewSummaryWriter(main),
		Events:   bus,
		Models:   fsEnvResolver{provider},
		Builder:  prompt.NewBuilder(tokenizer.NewHeuristic(), prompt.DefaultLimits()),
		Tools:    toolRunner,
		Catalog:  toolRunner,
		Gate:     gate,
		MaxSteps: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessSvc := service.NewSessionService(main, sessions)
	sess, err := sessSvc.Create(ctx, "fs-approval", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return &fsApprovalEnv{
		engine: engine, main: main, sessions: sessions, toolHost: toolHost,
		provider: provider, toolReg: toolReg, sessionID: sess.ID, gate: gate,
		snaps: snaps, dataDir: dir,
	}
}

// fsEnvResolver 固定 provider（agent.ModelResolver 端口）。
type fsEnvResolver struct{ p llm.Provider }

func (r fsEnvResolver) Resolve(_ context.Context, modelRef string) (llm.Provider, string, error) {
	if modelRef == "" || modelRef == "default" {
		modelRef = "test-model"
	}
	return r.p, modelRef, nil
}

// TestFS10_4WriteApprovalDenyReasonBack 写入触发审批，拒绝原因回传模型，Run 不中断。
func TestFS10_4WriteApprovalDenyReasonBack(t *testing.T) {
	env := newFSApprovalEnv(t, time.Second, [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{ID: "call-1", Name: "write_file",
			ArgumentsDelta: `{"path":"config.yaml","content":"key: value\n"}`}}},
		{{Delta: "收到拒绝，改用其他方案"}, {Usage: &llm.Usage{TotalTokens: 5}}},
	})
	// 预建 Run 行（真实链路由 RunService.CreateRunAs 创建）
	sdb0, err := env.sessions.Acquire(context.Background(), env.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sdb0.Runs().Create(context.Background(), &model.Run{
		ID: "run-1", Status: model.RunPending, ModelRef: "test-model", Executor: model.ExecutorServer,
	}); err != nil {
		t.Fatal(err)
	}
	// confirm 模式：write 需审批
	if err := env.engine.Execute(context.Background(), agent.RunRequest{
		RunID: "run-1", SessionID: env.sessionID, UserInput: "写配置",
		ModelRef: "test-model", AccessMode: approval.ModeConfirm,
		SystemBase: "测试",
	}); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	// ① Run 正常完成（拒绝不中断）
	sdb, err := env.sessions.Acquire(context.Background(), env.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	run, _ := sdb.Runs().Get(context.Background(), "run-1")
	if run == nil || run.Status != model.RunCompleted {
		t.Fatalf("拒绝后 Run 应完成: %+v", run)
	}
	// ② 工具事件 denied + 原因
	evs, _ := sdb.ToolEvents().ListByRun(context.Background(), "run-1")
	if len(evs) != 1 || evs[0].Status != model.ToolDenied {
		t.Fatalf("工具事件应为 denied: %+v", evs)
	}
	if !strings.Contains(evs[0].Error, "超时") && !strings.Contains(evs[0].Error, "拒绝") {
		t.Fatalf("拒绝原因缺失: %q", evs[0].Error)
	}
	// ③ 文件未落盘（拒绝即不执行）
	if exists, _ := env.toolHost.Workspace().Exists(context.Background(), "config.yaml"); exists {
		t.Fatal("被拒的写入不得落盘")
	}
	// ④ 拒绝原因作为 tool 消息回传模型（第二轮上下文）
	msgs, _ := sdb.Messages().List(context.Background(), 0, 100)
	deniedBack := false
	for _, m := range msgs {
		if m.Role == model.RoleTool && strings.Contains(m.Content, "denied:") {
			deniedBack = true
		}
	}
	if !deniedBack {
		t.Fatal("拒绝原因必须回传模型（role=tool 带 denied:）")
	}
}

// TestFS10_5AllowOnceNoSessionRule 放行语义：allow_once 不写会话规则（§10-5 作用域）。
func TestFS10_5AllowOnceNoSessionRule(t *testing.T) {
	env := newFSApprovalEnv(t, 10*time.Second, [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{ID: "call-1", Name: "write_file",
			ArgumentsDelta: `{"path":"notes.md","content":"# 笔记\n"}`}}},
		{{Delta: "已写入"}, {Usage: &llm.Usage{TotalTokens: 3}}},
	})
	// 预建 Run 行
	sdb0, err := env.sessions.Acquire(context.Background(), env.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sdb0.Runs().Create(context.Background(), &model.Run{
		ID: "run-2", Status: model.RunPending, ModelRef: "test-model", Executor: model.ExecutorServer,
	}); err != nil {
		t.Fatal(err)
	}
	gate := env.gate
	done := make(chan struct{})
	go func() {
		_ = env.engine.Execute(context.Background(), agent.RunRequest{
			RunID: "run-2", SessionID: env.sessionID, UserInput: "写笔记",
			ModelRef: "test-model", AccessMode: approval.ModeConfirm,
			SystemBase: "测试",
		})
		close(done)
	}()
	// 等审批登记 → 模拟用户「允许一次」
	deadline := time.Now().Add(3 * time.Second)
	for gate.PendingCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if gate.PendingCount() == 0 {
		t.Fatal("审批未登记")
	}
	for _, id := range gate.PendingIDs() {
		gate.Resolve(id, approval.AllowOnce)
	}
	<-done

	sdb, _ := env.sessions.Acquire(context.Background(), env.sessionID)
	run, _ := sdb.Runs().Get(context.Background(), "run-2")
	if run == nil || run.Status != model.RunCompleted {
		t.Fatalf("放行后 Run 应完成: %+v", run)
	}
	if exists, _ := env.toolHost.Workspace().Exists(context.Background(), "notes.md"); !exists {
		t.Fatal("放行后文件应写入")
	}
	// allow_once 不写会话规则（§10-5：allow_session 才写，且仅作用于该会话）
	rules, _ := sdb.ApprovalRules().List(context.Background())
	if len(rules) != 0 {
		t.Fatalf("allow_once 不得写会话规则: %+v", rules)
	}
}
