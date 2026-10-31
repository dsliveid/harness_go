package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"harness-go/internal/agent"
	"harness-go/internal/approval"
	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/model"
	"harness-go/internal/prompt"
	"harness-go/internal/store"
	"harness-go/internal/tokenizer"
	"harness-go/pkg/toolapi"
)

// newChildTestEnv 构造嵌套 Run 测试环境（fake provider 回显，无工具）。
func newChildTestEnv(t *testing.T) (*ChildRunner, *store.MainStore, *store.SessionStore, *llm.FakeProvider) {
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

	provider := llm.NewFakeProvider()
	provider.Delay = 0
	engine, err := agent.New(agent.Deps{
		Sessions: NewSessionProvider(sessions),
		Summary:  NewSummaryWriter(main),
		Events:   bus,
		Models:   childTestResolver{provider},
		Builder:  prompt.NewBuilder(tokenizer.NewHeuristic(), prompt.DefaultLimits()),
		Gate:     approval.NewGate(time.Second),
		MaxSteps: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	cr := NewChildRunner(ChildRunnerOptions{
		Sessions:   sessions,
		Engine:     engine,
		Main:       main,
		SystemBase: "你是测试助手",
	})
	return cr, main, sessions, provider
}

// TestChildRunnerSpawnCompletes 嵌套 Run：真实落库 + Output 取子 Run 最终回答。
func TestChildRunnerSpawnCompletes(t *testing.T) {
	cr, main, sessions, _ := newChildTestEnv(t)
	ctx := context.Background()
	if _, err := NewSessionService(main, sessions).Create(ctx, "嵌套测试", "", ""); err != nil {
		t.Fatal(err)
	}
	sess, _ := main.Sessions().List(ctx, 10, 0)
	if len(sess) == 0 {
		t.Fatal("会话未创建")
	}
	child, err := cr.Spawn(ctx, toolapi.SpawnRequest{
		ParentRunID: "parent-run-1",
		SessionID:   sess[0].ID,
		Input:       "子任务分析",
	})
	if err != nil {
		t.Fatalf("Spawn 失败: %v", err)
	}
	res, err := child.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait 失败: %v", err)
	}
	if res.Status != toolapi.ChildCompleted {
		t.Fatalf("应完成，得到 %v（err=%s）", res.Status, res.Error)
	}
	if !strings.Contains(res.Output, "[fake]") {
		t.Fatalf("Output 应为子 Run 的最终回答: %q", res.Output)
	}
	// 子 Run 落库可见（UI 可观测）
	sdb, err := sessions.Acquire(ctx, sess[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := sdb.Runs().Get(ctx, res.RunID)
	if err != nil || run.Status != model.RunCompleted {
		t.Fatalf("子 Run 落库状态错误: %+v %v", run, err)
	}
	// Get 端口：在途可查（已终态的句柄仍可经 Spawn 返回值持有；这里验证不 panic）
	if _, ok := cr.Get(res.RunID); ok {
		t.Log("终态后仍在 live 表（可接受，unregister 由 goroutine 收尾清理）")
	}
}

// TestChildRunnerRecursionForbidden 禁递归：子**在途**时派的孙必须拒绝。
// （子终结后 parentOf 清理——已终结的 Run 无法再执行工具，无递归面。）
func TestChildRunnerRecursionForbidden(t *testing.T) {
	cr, main, sessions, provider := newChildTestEnv(t)
	provider.Delay = 2 * time.Second // 拉长子 Run 在途窗口，保证断言确定性
	ctx := context.Background()
	if _, err := NewSessionService(main, sessions).Create(ctx, "递归测试", "", ""); err != nil {
		t.Fatal(err)
	}
	sess, _ := main.Sessions().List(ctx, 10, 0)

	// 第一层：父 → 子（合法，且仍在途）
	child, err := cr.Spawn(ctx, toolapi.SpawnRequest{
		ParentRunID: "parent-run-1", SessionID: sess[0].ID, Input: "第一层",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 第二层：子 → 孙（子在途，深度链已登记 → 必须拒绝）
	_, err = cr.Spawn(ctx, toolapi.SpawnRequest{
		ParentRunID: child.ID(), SessionID: sess[0].ID, Input: "第二层",
	})
	if err == nil || !strings.Contains(err.Error(), "禁递归") {
		t.Fatalf("子的子必须被拒绝（禁递归）: %v", err)
	}
	// 取消并等待子 goroutine 收尾（否则 TempDir 清理与持库 goroutine 竞态）
	_ = child.Cancel()
	_, _ = child.Wait(ctx)
}

// TestChildRunnerDepthBoundary MaxDepth=3：深度 2 的 g1 可派生（新深度 3），深度 3 的 g2 不可。
func TestChildRunnerDepthBoundary(t *testing.T) {
	cr, mainStore, sessions, _ := newChildTestEnv(t)
	cr.opts.MaxDepth = 3
	ctx := context.Background()
	if _, err := NewSessionService(mainStore, sessions).Create(ctx, "深度边界", "", ""); err != nil {
		t.Fatal(err)
	}
	list, _ := mainStore.Sessions().List(ctx, 10, 0)
	// 手工登记深度链：g1（深度 2，父=top）、g2（深度 3，父=g1）
	cr.mu.Lock()
	cr.parentOf["g1"] = "top"
	cr.parentOf["g2"] = "g1"
	cr.mu.Unlock()
	// g1（深度 2）派生 → 新深度 3 ≤ MaxDepth → 放行
	g3, err := cr.Spawn(ctx, toolapi.SpawnRequest{
		ParentRunID: "g1", SessionID: list[0].ID, Input: "第三层",
	})
	if err != nil {
		t.Fatalf("深度 3（MaxDepth=3）应放行: %v", err)
	}
	_, _ = g3.Wait(ctx) // 等 goroutine 收尾（TempDir 清理不与持库竞争）
	// g2（深度 3）派生 → 新深度 4 > MaxDepth → 拒绝
	_, err = cr.Spawn(ctx, toolapi.SpawnRequest{
		ParentRunID: "g2", SessionID: list[0].ID, Input: "第四层",
	})
	if err == nil || !strings.Contains(err.Error(), "禁递归") {
		t.Fatalf("深度 4 必须拒绝: %v", err)
	}
}

// childTestResolver 固定返回注入的 provider（agent.ModelResolver 端口实现）。
type childTestResolver struct{ p llm.Provider }

func (r childTestResolver) Resolve(_ context.Context, modelRef string) (llm.Provider, string, error) {
	if modelRef == "" {
		modelRef = "test-model"
	}
	return r.p, modelRef, nil
}
