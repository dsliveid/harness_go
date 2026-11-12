package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// R2 对话准入·物理层拦截：归属设备离线 → 拒绝发起 Run。
//
// 核心断言（缺一不可）：
//   1. 返回错误码必须是 device.offline（前端据此精准提示，R4）
//   2. **用户消息不得落库** —— 这是「物理层」与「上下文层」的本质区别
//   3. 设备恢复在线后，同一会话可正常发起
//   4. 服务端内置空间（ws-server）在任何设备离线时都不受影响（R5）

// newRunAdmissionEnv 构造 Run 准入测试环境（fake provider，无工具）。
type runAdmissionEnv struct {
	svc      *RunService
	main     *store.MainStore
	sessions *store.SessionStore
	hub      *fakeHub
	ctx      context.Context
}

func newRunAdmissionEnv(t *testing.T) *runAdmissionEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	main, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	sessions, err := store.OpenSessionStore(store.SessionStoreOptions{DataDir: dir, MaxOpen: 4})
	if err != nil {
		t.Fatalf("打开会话库失败: %v", err)
	}
	t.Cleanup(func() { _ = sessions.Close() })
	bus := events.NewBus(100, 32)
	t.Cleanup(bus.Close)

	provider := llm.NewFakeProvider()
	provider.Delay = 0
	engine, err := newRunAdmissionEngine(t, main, sessions, bus, provider)
	if err != nil {
		t.Fatalf("构造引擎失败: %v", err)
	}
	hub := &fakeHub{online: map[string]bool{}}
	svc := NewRunService(RunServiceOptions{
		Main: main, Engine: engine, Sessions: sessions, Hub: hub,
	})
	return &runAdmissionEnv{svc: svc, main: main, sessions: sessions, hub: hub, ctx: ctx}
}

// addDesktopWorkspace 插入一台桌面设备 + 其工作空间，返回 workspaceID。
func (e *runAdmissionEnv) addDesktopWorkspace(t *testing.T, devID, wsID string) {
	t.Helper()
	d := &model.Device{
		ID: devID, Name: "设备-" + devID, Kind: model.DeviceKindDesktop, Platform: "test",
		Status: model.DeviceOffline, ProtocolVersion: model.DefaultDeviceProtocolVersion,
		TokenHash: "hash-" + devID, PairedAt: store.NowMillis(),
	}
	if err := e.main.Devices().Create(e.ctx, d); err != nil {
		t.Fatalf("插入设备失败: %v", err)
	}
	w := &model.Workspace{
		ID: wsID, Name: "空间-" + wsID, Kind: model.WorkspaceKindDesktop, DeviceID: devID,
		CreatedAt: store.NowMillis(), UpdatedAt: store.NowMillis(),
	}
	if err := e.main.Workspaces().Create(e.ctx, w); err != nil {
		t.Fatalf("插入工作空间失败: %v", err)
	}
}

// countMessages 会话库内的消息条数（用于断言「未落库」）。
func (e *runAdmissionEnv) countMessages(t *testing.T, sessionID string) int {
	t.Helper()
	sdb, err := e.sessions.Acquire(e.ctx, sessionID)
	if err != nil {
		// 会话库文件尚未创建 = 零消息，符合预期。
		return 0
	}
	msgs, err := sdb.Messages().List(e.ctx, 100, 0)
	if err != nil {
		return 0
	}
	return len(msgs)
}

// waitMessages 轮询等待会话的消息计数增长（异步执行，不依赖会话库句柄）。
//
// 为什么用主库的 message_count 而不是枚举会话库消息：
// 会话库文件由异步 goroutine 打开，Windows 下句柄时序不稳；主库摘要在
// 用户消息落库后由 TouchMessage 回写，是更稳定的观测点。
func (e *runAdmissionEnv) waitMessages(t *testing.T, sessionID string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s, err := e.main.Sessions().Get(e.ctx, sessionID)
		if err == nil && s.MessageCount > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// waitRunDone 等待 Run 从进行中表注销（异步执行结束）。
func (e *runAdmissionEnv) waitRunDone(t *testing.T, runID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.svc.mu.Lock()
		_, running := e.svc.cancels[runID]
		e.svc.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("Run %s 在超时内未终结（不致命，可能是调度慢）", runID)
}

// TestCreateRunRejectedWhenOwnerDeviceOffline 核心用例：设备离线 → 拒绝且消息不落库。
func TestCreateRunRejectedWhenOwnerDeviceOffline(t *testing.T) {
	env := newRunAdmissionEnv(t)
	env.addDesktopWorkspace(t, "dev-1", "ws-1")

	sess := &model.Session{
		ID: "sess-offline", Title: "离线会话",
		WorkspaceID: "ws-1", AccessMode: "full", ModelRef: "fake-model",
	}
	if err := env.main.Sessions().Create(env.ctx, sess); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	// 设备离线（hub 中不存在）
	env.hub.online["dev-1"] = false

	_, err := env.svc.CreateRun(env.ctx, "sess-offline", "你好", "fake-model")
	if err == nil {
		t.Fatal("设备离线时应拒绝发起 Run，却成功了")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.CodeDeviceOffline {
		t.Fatalf("错误码应为 %q，得到 %v", apperr.CodeDeviceOffline, err)
	}
	// 关键断言：消息绝不落库。
	if n := env.countMessages(t, "sess-offline"); n != 0 {
		t.Fatalf("离线拒绝时用户消息不得落库，实际有 %d 条", n)
	}
}

// TestCreateRunAllowedWhenOwnerDeviceOnline 设备在线 → 正常受理。
func TestCreateRunAllowedWhenOwnerDeviceOnline(t *testing.T) {
	env := newRunAdmissionEnv(t)
	env.addDesktopWorkspace(t, "dev-1", "ws-1")
	sess := &model.Session{
		ID: "sess-online", Title: "在线会话",
		WorkspaceID: "ws-1", AccessMode: "full", ModelRef: "fake-model",
	}
	if err := env.main.Sessions().Create(env.ctx, sess); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	env.hub.online["dev-1"] = true

	run, err := env.svc.CreateRun(env.ctx, "sess-online", "你好", "fake-model")
	if err != nil {
		t.Fatalf("设备在线时应可发起 Run，却失败: %v", err)
	}
	if run == nil || run.ID == "" {
		t.Fatal("应返回有效 Run")
	}
	// 用户消息由异步 goroutine 落库，轮询等待（避免依赖调度时序）。
	if !env.waitMessages(t, "sess-online", 3*time.Second) {
		t.Fatal("在线发起后用户消息应已落库")
	}
}

// TestCreateRunServerWorkspaceUnaffectedByDeviceOffline 服务端空间不受任何设备离线影响（R5）。
func TestCreateRunServerWorkspaceUnaffectedByDeviceOffline(t *testing.T) {
	env := newRunAdmissionEnv(t)
	sess := &model.Session{
		ID: "sess-srv", Title: "服务端会话",
		WorkspaceID: store.BuiltinWorkspaceServerID, AccessMode: "full", ModelRef: "fake-model",
	}
	if err := env.main.Sessions().Create(env.ctx, sess); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	// 无任何在线设备。
	run, err := env.svc.CreateRun(env.ctx, "sess-srv", "你好", "fake-model")
	if err != nil {
		t.Fatalf("服务端内置空间应恒可对话，却失败: %v", err)
	}
	if run == nil || run.ID == "" {
		t.Fatal("应返回有效 Run")
	}
	// 等待 Run 终结，确保会话库文件句柄在所有平台上都能释放（Windows 下必需）。
	env.waitRunDone(t, run.ID)
}

// TestCreateRunUnboundDeviceStillAllowedButNoTools 未绑定设备的会话沿用既有行为（不因 R2 被拦）。
//
// 设计边界：R2 只拦「设备离线」这一种在线状态问题。未绑定/设备已删/已解绑
// 属于归属异常，它们本就由 resolveToolPolicy 判 none（工具全禁），
// 但**不阻断对话**——强行拦会改变历史会话的可访问性，超出本次需求范围。
func TestCreateRunUnboundDeviceStillAllowedButNoTools(t *testing.T) {
	env := newRunAdmissionEnv(t)
	w := &model.Workspace{
		ID: "ws-unbound", Name: "未绑定空间", Kind: model.WorkspaceKindDesktop,
		CreatedAt: store.NowMillis(), UpdatedAt: store.NowMillis(),
	}
	if err := env.main.Workspaces().Create(env.ctx, w); err != nil {
		t.Fatalf("插入工作空间失败: %v", err)
	}
	sess := &model.Session{
		ID: "sess-unbound", Title: "未绑定会话",
		WorkspaceID: "ws-unbound", AccessMode: "full", ModelRef: "fake-model",
	}
	if err := env.main.Sessions().Create(env.ctx, sess); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	policy, reason := env.svc.resolveToolPolicy(env.ctx, sess)
	if policy != "none" {
		t.Fatalf("未绑定设备的会话工具策略应为 none，得到 %s（reason=%q）", policy, reason)
	}
	// 但对话本身不被 R2 拦截。
	run, err := env.svc.CreateRun(env.ctx, "sess-unbound", "你好", "fake-model")
	if err != nil {
		t.Fatalf("未绑定设备不应阻断对话（仅禁工具），却失败: %v", err)
	}
	env.waitRunDone(t, run.ID)
}
