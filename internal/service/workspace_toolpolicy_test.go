package service

import (
	"context"
	"testing"
	"time"

	"harness-go/internal/agent"
	"harness-go/internal/model"
	"harness-go/internal/prompt"
	"harness-go/internal/store"
)

// ============ 工作空间能力天花板（ToolPolicy 三态）============
//
// 本文件锁死「未绑定 / 离线 / 设备已删除 / 已解绑 / 移动端 / 内置空间」六种情形
// 分别得到什么工具能力。这是 D1~D4 决策的可执行契约：
//   - 无可用执行宿主 → none（零工具）：因为任何工具都只能落到服务端本地执行（D8 禁止）；
//   - 移动端      → readonly：可对话 + 只读，禁写禁命令；
//   - 在线设备/内置空间 → full。
//
// 🚨 fail-closed 铁律：任何「查不到 / 不确定」一律从严，绝不因为缺信息而放行工具。

// fakeHub 设备在线判定桩（可控返回，不依赖真实 WS 连接）。
type fakeHub struct{ online map[string]bool }

func (h *fakeHub) Online(deviceID string) bool { return h.online[deviceID] }

// policyEnv 能力天花板测试环境。
type policyEnv struct {
	main *store.MainStore
	hub  *fakeHub
	svc  *RunService
	ctx  context.Context
}

func newPolicyEnv(t *testing.T) *policyEnv {
	t.Helper()
	main, err := store.OpenMain(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	hub := &fakeHub{online: map[string]bool{}}
	// 只需要 main 与 hub 就能测能力判定：engine/sessions 留空，
	// 因为 resolveToolPolicy 不依赖它们（依赖面最小化本身就是可测性收益）。
	svc := NewRunService(RunServiceOptions{Main: main, Hub: hub})
	return &policyEnv{main: main, hub: hub, svc: svc, ctx: context.Background()}
}

// addDevice 插入一台设备并返回其 ID（令牌摘要用固定串，避免随机）。
func (e *policyEnv) addDevice(t *testing.T, id string, kind model.DeviceKind) {
	t.Helper()
	d := &model.Device{
		ID: id, Name: "设备-" + id, Kind: kind, Platform: "test",
		Status: model.DeviceOffline, ProtocolVersion: model.DefaultDeviceProtocolVersion,
		TokenHash: "hash-" + id, PairedAt: store.NowMillis(),
	}
	if err := e.main.Devices().Create(e.ctx, d); err != nil {
		t.Fatalf("插入设备失败: %v", err)
	}
}

// addWorkspace 插入一个工作空间并返回其 ID。
func (e *policyEnv) addWorkspace(t *testing.T, id string, kind model.WorkspaceKind, deviceID string) {
	t.Helper()
	w := &model.Workspace{
		ID: id, Name: id, Kind: kind, DeviceID: deviceID,
		CreatedAt: store.NowMillis(), UpdatedAt: store.NowMillis(),
	}
	if err := e.main.Workspaces().Create(e.ctx, w); err != nil {
		t.Fatalf("插入工作空间失败: %v", err)
	}
}

func (e *policyEnv) policy(t *testing.T, wsID string) (agent.ToolPolicy, string) {
	t.Helper()
	return e.svc.resolveToolPolicy(e.ctx, &model.Session{ID: "sess-1", WorkspaceID: wsID})
}

// 内置服务端空间：**有项目**时 full（有明确工作目录与执行意图）、**无项目**时 readonly（纯对话）。
//
// R1b 改造说明：此前该空间无条件 full，导致「纯对话」能在服务端宿主机上执行命令。
// 现在按 project_id 细分——有项目 = 用户已声明执行意图，保持 full；无项目 = 纯对话，
// 降为 readonly。这与「级联删除不得把会话迁到 ws-server 防提权」的铁律同源。
func TestPolicyBuiltinServerWorkspaceIsFull(t *testing.T) {
	env := newPolicyEnv(t)
	// 有项目 → full
	p, reason := env.svc.resolveToolPolicy(env.ctx, &model.Session{
		ID: "sess-1", WorkspaceID: store.BuiltinWorkspaceServerID, ProjectID: "proj-1",
	})
	if p != agent.ToolPolicyFull {
		t.Fatalf("内置服务端空间（有项目）应为 full，得到 %s（reason=%q）", p, reason)
	}
	if reason != "" {
		t.Fatalf("full 策略不应带拒绝文案，得到 %q", reason)
	}
}

// 内置服务端空间 + 无项目 = 纯对话（R1b）：readonly，禁写禁命令。
func TestPolicyBuiltinServerWorkspacePureChatIsReadonly(t *testing.T) {
	env := newPolicyEnv(t)
	p, reason := env.policy(t, store.BuiltinWorkspaceServerID)
	if p != agent.ToolPolicyReadonly {
		t.Fatalf("内置服务端空间（纯对话/无项目）应为 readonly，得到 %s（reason=%q）", p, reason)
	}
	if reason != policyReasonPureChat {
		t.Fatalf("纯对话拒绝文案应为 %q，得到 %q", policyReasonPureChat, reason)
	}
}

// 纯对话会话（workspace 为空）：readonly——可读不可写。
//
// 这是本次改造补上的漏洞：旧 DenyTools 只拦 command+danger，
// 纯对话会话能偷偷 write_file 改文件。
func TestPolicyNoWorkspaceIsReadonly(t *testing.T) {
	env := newPolicyEnv(t)
	p, reason := env.policy(t, "")
	if p != agent.ToolPolicyReadonly {
		t.Fatalf("未绑定执行空间的会话应为 readonly，得到 %s", p)
	}
	if reason != policyReasonNoWorkspace {
		t.Fatalf("拒绝文案应为 %q，得到 %q", policyReasonNoWorkspace, reason)
	}
	if p.Allows(model.RiskWrite) {
		t.Fatal("readonly 必须拦下 write 级工具（旧 DenyTools 缺陷）")
	}
	if !p.Allows(model.RiskReadonly) {
		t.Fatal("readonly 应放行只读工具")
	}
}

// 未绑定设备的非内置空间 → none（零工具）。
func TestPolicyUnboundWorkspaceIsNone(t *testing.T) {
	env := newPolicyEnv(t)
	env.addWorkspace(t, "ws-unbound", model.WorkspaceKindDesktop, "")
	p, reason := env.policy(t, "ws-unbound")
	if p != agent.ToolPolicyNone {
		t.Fatalf("未绑定设备的空间应为 none，得到 %s", p)
	}
	if reason != policyReasonUnbound {
		t.Fatalf("拒绝文案应为 %q，得到 %q", policyReasonUnbound, reason)
	}
}

// 设备在线 → full（desktop 空间）。
func TestPolicyOnlineDeviceIsFull(t *testing.T) {
	env := newPolicyEnv(t)
	env.addDevice(t, "dev-a", model.DeviceKindDesktop)
	env.addWorkspace(t, "ws-a", model.WorkspaceKindDesktop, "dev-a")
	env.hub.online["dev-a"] = true

	p, reason := env.policy(t, "ws-a")
	if p != agent.ToolPolicyFull {
		t.Fatalf("在线设备空间应为 full，得到 %s（reason=%q）", p, reason)
	}
}

// 设备离线 → none（🚨 D3：绝不降级到服务端本地执行）。
func TestPolicyOfflineDeviceIsNone(t *testing.T) {
	env := newPolicyEnv(t)
	env.addDevice(t, "dev-b", model.DeviceKindDesktop)
	env.addWorkspace(t, "ws-b", model.WorkspaceKindDesktop, "dev-b")
	// 不设 online → 离线

	p, reason := env.policy(t, "ws-b")
	if p != agent.ToolPolicyNone {
		t.Fatalf("离线设备空间应为 none，得到 %s", p)
	}
	if reason != policyReasonOffline {
		t.Fatalf("拒绝文案应为 %q，得到 %q", policyReasonOffline, reason)
	}
}

// Hub 未注入（nil）→ 设备空间一律 none（fail-closed，绝不假设在线）。
func TestPolicyNilHubIsNone(t *testing.T) {
	env := newPolicyEnv(t)
	env.addDevice(t, "dev-c", model.DeviceKindDesktop)
	env.addWorkspace(t, "ws-c", model.WorkspaceKindDesktop, "dev-c")
	env.svc.hub = nil

	p, _ := env.policy(t, "ws-c")
	if p != agent.ToolPolicyNone {
		t.Fatalf("Hub 缺失时设备空间应为 none（fail-closed），得到 %s", p)
	}
}

// 设备记录已删除（物理删除）→ none，且文案是「已删除」而非「离线」。
func TestPolicyDeviceMissingIsNone(t *testing.T) {
	env := newPolicyEnv(t)
	env.addWorkspace(t, "ws-missing", model.WorkspaceKindDesktop, "dev-gone")
	// 只建空间不建设备：模拟设备已被物理删除、空间悬空

	p, reason := env.policy(t, "ws-missing")
	if p != agent.ToolPolicyNone {
		t.Fatalf("设备已删除的空间应为 none，得到 %s", p)
	}
	if reason != policyReasonDeviceMissing {
		t.Fatalf("拒绝文案应为「已删除」(%q)，得到 %q（不能与离线混淆）", policyReasonDeviceMissing, reason)
	}
}

// 设备已解绑（revoked_at 置位）→ none，文案是「已解绑」。
func TestPolicyDeviceRevokedIsNone(t *testing.T) {
	env := newPolicyEnv(t)
	env.addDevice(t, "dev-revoked", model.DeviceKindDesktop)
	env.addWorkspace(t, "ws-revoked", model.WorkspaceKindDesktop, "dev-revoked")
	if err := env.main.Devices().Revoke(env.ctx, "dev-revoked", store.NowMillis()); err != nil {
		t.Fatalf("吊销设备失败: %v", err)
	}
	env.hub.online["dev-revoked"] = true // 即使 Hub 里还挂着连接，也必须按已解绑拒绝

	p, reason := env.policy(t, "ws-revoked")
	if p != agent.ToolPolicyNone {
		t.Fatalf("已解绑设备的空间应为 none，得到 %s", p)
	}
	if reason != policyReasonDeviceRevoked {
		t.Fatalf("拒绝文案应为「已解绑」(%q)，得到 %q", policyReasonDeviceRevoked, reason)
	}
}

// 移动端在线 → readonly（可对话+只读，禁写禁命令）。
func TestPolicyMobileOnlineIsReadonly(t *testing.T) {
	env := newPolicyEnv(t)
	env.addDevice(t, "dev-phone", model.DeviceKindMobile)
	env.addWorkspace(t, "ws-phone", model.WorkspaceKindMobile, "dev-phone")
	env.hub.online["dev-phone"] = true

	p, reason := env.policy(t, "ws-phone")
	if p != agent.ToolPolicyReadonly {
		t.Fatalf("移动端空间应为 readonly，得到 %s", p)
	}
	if reason != policyReasonMobile {
		t.Fatalf("拒绝文案应为 %q，得到 %q", policyReasonMobile, reason)
	}
}

// 悬空 workspace_id（空间已被删除）→ none，绝不放宽。
func TestPolicyDanglingWorkspaceIsNone(t *testing.T) {
	env := newPolicyEnv(t)
	p, _ := env.policy(t, "ws-does-not-exist")
	if p != agent.ToolPolicyNone {
		t.Fatalf("悬空 workspace_id 应为 none，得到 %s", p)
	}
}

// executionDeviceID：工具执行面必须落在会话绑定的设备上，内置空间才是空串。
func TestExecutionDeviceID(t *testing.T) {
	env := newPolicyEnv(t)
	env.addDevice(t, "dev-x", model.DeviceKindDesktop)
	env.addWorkspace(t, "ws-x", model.WorkspaceKindDesktop, "dev-x")

	if got := env.svc.executionDeviceID(env.ctx, &model.Session{WorkspaceID: "ws-x"}); got != "dev-x" {
		t.Fatalf("应返回绑定设备 dev-x，得到 %q", got)
	}
	if got := env.svc.executionDeviceID(env.ctx, &model.Session{WorkspaceID: store.BuiltinWorkspaceServerID}); got != "" {
		t.Fatalf("内置空间应返回空串（服务端本地执行），得到 %q", got)
	}
	if got := env.svc.executionDeviceID(env.ctx, &model.Session{WorkspaceID: ""}); got != "" {
		t.Fatalf("纯对话应为空串，得到 %q", got)
	}
}

// filterTools：none 策略必须**全量清空**（且不依赖 catalog 是否装配）。
func TestFilterToolsNoneClearsAllEvenWithoutCatalog(t *testing.T) {
	env := newPolicyEnv(t) // catalog 为 nil
	refs := []prompt.ToolRef{{Name: "read_file"}, {Name: "write_file"}}
	if got := env.svc.filterTools(refs, agent.ToolPolicyNone); len(got) != 0 {
		t.Fatalf("none 策略应清空全部工具（catalog 缺失也不得有残留），得到 %d 条", len(got))
	}
	// full 且 catalog 缺失：保守保留全部（不能凭工具名猜风险）
	if got := env.svc.filterTools(refs, agent.ToolPolicyFull); len(got) != len(refs) {
		t.Fatalf("catalog 缺失时 full 应保留全部工具，得到 %d 条", len(got))
	}
}

// checkKindMatchesDevice：kind 错配必须 400（防能力天花板错位/提权）。
func TestCheckKindMatchesDevice(t *testing.T) {
	desktop := &model.Device{ID: "d1", Kind: model.DeviceKindDesktop}
	mobile := &model.Device{ID: "d2", Kind: model.DeviceKindMobile}
	cli := &model.Device{ID: "d3", Kind: model.DeviceKindCLI}

	if err := checkKindMatchesDevice(model.WorkspaceKindDesktop, desktop); err != nil {
		t.Fatalf("desktop↔desktop 应通过，得到 %v", err)
	}
	if err := checkKindMatchesDevice(model.WorkspaceKindMobile, mobile); err != nil {
		t.Fatalf("mobile↔mobile 应通过，得到 %v", err)
	}
	// mobile 空间绑 desktop 设备：能力天花板错位。
	if err := checkKindMatchesDevice(model.WorkspaceKindMobile, desktop); err == nil {
		t.Fatal("mobile 空间绑 desktop 设备必须拒绝")
	}
	// desktop 空间绑 mobile 设备：会让移动端拿到写能力（提权），必须拒绝。
	if err := checkKindMatchesDevice(model.WorkspaceKindDesktop, mobile); err == nil {
		t.Fatal("desktop 空间绑 mobile 设备必须拒绝（提权风险）")
	}
	// cli 无对应 WorkspaceKind：不阻断（协议 §11.1 不做映射）。
	if err := checkKindMatchesDevice(model.WorkspaceKindDesktop, cli); err != nil {
		t.Fatalf("无对应关系的设备形态不应阻断，得到 %v", err)
	}
}

// 保持 time 包被引用（部分环境下 fakeHub 用例需要显式时钟语义时使用）。
var _ = time.Second
