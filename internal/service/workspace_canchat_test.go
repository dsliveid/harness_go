package service

import (
	"testing"

	"harness-go/internal/model"
	"harness-go/internal/store"
)

// ============ can_chat 派生（2026-11 口径 1 定案：设备离线即禁对话）============
//
// 口径 1：归属执行面（设备）离线 → **对话不可发起**，仅可查看历史。
// 与之严格区分的三类「零工具但可对话」：未绑定设备 / 设备已删除 / 设备已解绑。
//
// 🚨 为什么必须单测锁死：历史上 CanChat 恒为 true（`CanChat bool` 字面量初始化 + 注释
// 写着「v2：无可用宿主也允许对话」），前端据此不禁用输入框，于是「设备离线」被实现成
// 「能聊但没有工具」——与产品最终裁决相反。这类字段一旦漂移，前端只会静默放行，
// 而服务端 409 只在**点击发送之后**才触发，体验上等同于无拦截。

// fakeCanChatHub 在线判定桩：可控返回，不依赖真实 WS 连接。
type fakeCanChatHub struct{ online map[string]bool }

func (h *fakeCanChatHub) Online(deviceID string) bool { return h.online[deviceID] }

// newCanChatEnv 建「主库 + 可控 Hub」的最小工作空间用例环境。
func newCanChatEnv(t *testing.T) (*WorkspaceService, *fakeCanChatHub, *store.MainStore) {
	t.Helper()
	main, err := store.OpenMain(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	hub := &fakeCanChatHub{online: map[string]bool{}}
	return NewWorkspaceService(main, hub), hub, main
}

// TestCanChatFalseWhenOwnerDeviceOffline 核心用例：归属设备离线 → can_chat=false。
func TestCanChatFalseWhenOwnerDeviceOffline(t *testing.T) {
	svc, hub, main := newCanChatEnv(t)
	ctx := t.Context()
	addBindDevice(t, main, ctx, "dev-off", model.DeviceKindDesktop)
	w, err := svc.Create(ctx, CreateWorkspaceInput{Name: "离线空间", Kind: "desktop", DeviceID: "dev-off"})
	if err != nil {
		t.Fatalf("创建工作空间失败: %v", err)
	}
	hub.online["dev-off"] = false // 明确离线

	v, err := svc.Get(ctx, w.ID)
	if err != nil {
		t.Fatalf("取工作空间视图失败: %v", err)
	}
	if v.CanChat {
		t.Fatal("归属设备离线时 can_chat 必须为 false（口径 1：仅可查看历史）")
	}
	if v.Online {
		t.Fatal("归属设备离线时 online 必须为 false")
	}
	if v.ToolPolicy != "none" {
		t.Fatalf("归属设备离线时 tool_policy 必须为 none，得到 %q", v.ToolPolicy)
	}
	if v.PolicyReason != policyReasonOffline {
		t.Fatalf("文案应为 %q，得到 %q", policyReasonOffline, v.PolicyReason)
	}
}

// TestCanChatTrueWhenOwnerDeviceOnline 在线设备空间 → can_chat=true 且全工具。
func TestCanChatTrueWhenOwnerDeviceOnline(t *testing.T) {
	svc, hub, main := newCanChatEnv(t)
	ctx := t.Context()
	addBindDevice(t, main, ctx, "dev-on", model.DeviceKindDesktop)
	w, err := svc.Create(ctx, CreateWorkspaceInput{Name: "在线空间", Kind: "desktop", DeviceID: "dev-on"})
	if err != nil {
		t.Fatalf("创建工作空间失败: %v", err)
	}
	hub.online["dev-on"] = true

	v, err := svc.Get(ctx, w.ID)
	if err != nil {
		t.Fatalf("取工作空间视图失败: %v", err)
	}
	if !v.CanChat {
		t.Fatal("归属设备在线时 can_chat 必须为 true")
	}
	if v.ToolPolicy != "full" {
		t.Fatalf("桌面端在线空间应为 full，得到 %q", v.ToolPolicy)
	}
}

// TestCanChatTrueForUnboundWorkspace 未绑定设备的空间 → 仍可对话（只没有工具能力）。
//
// 这类空间与「离线」是**不同语义**：归属异常不是在线状态问题，强拦会改变历史会话
// 的可访问性，超出本次口径范围（见 docs 下一步工作计划 §0.3 表格）。
func TestCanChatTrueForUnboundWorkspace(t *testing.T) {
	svc, _, _ := newCanChatEnv(t)
	ctx := t.Context()
	w, err := svc.Create(ctx, CreateWorkspaceInput{Name: "手工空间", Kind: "desktop"})
	if err != nil {
		t.Fatalf("创建工作空间失败: %v", err)
	}

	v, err := svc.Get(ctx, w.ID)
	if err != nil {
		t.Fatalf("取工作空间视图失败: %v", err)
	}
	if !v.CanChat {
		t.Fatal("未绑定设备的空间仍可对话（零工具），can_chat 不得为 false")
	}
	if v.ToolPolicy != "none" {
		t.Fatalf("未绑定设备应为 none 工具策略，得到 %q", v.ToolPolicy)
	}
	if v.PolicyReason != policyReasonUnbound {
		t.Fatalf("文案应为 %q，得到 %q", policyReasonUnbound, v.PolicyReason)
	}
}

// TestCanChatTrueForRevokedDeviceWorkspace 设备已解绑 → 仍可对话（零工具）。
func TestCanChatTrueForRevokedDeviceWorkspace(t *testing.T) {
	svc, _, main := newCanChatEnv(t)
	ctx := t.Context()
	addBindDevice(t, main, ctx, "dev-rev", model.DeviceKindDesktop)
	w, err := svc.Create(ctx, CreateWorkspaceInput{Name: "解绑空间", Kind: "desktop", DeviceID: "dev-rev"})
	if err != nil {
		t.Fatalf("创建工作空间失败: %v", err)
	}
	if err := main.Devices().Revoke(ctx, "dev-rev", store.NowMillis()); err != nil {
		t.Fatalf("吊销设备失败: %v", err)
	}

	v, err := svc.Get(ctx, w.ID)
	if err != nil {
		t.Fatalf("取工作空间视图失败: %v", err)
	}
	if !v.CanChat {
		t.Fatal("设备已解绑的属归属异常（非在线状态问题），仍可对话，can_chat 不得为 false")
	}
	if v.PolicyReason != policyReasonDeviceRevoked {
		t.Fatalf("文案应为 %q，得到 %q", policyReasonDeviceRevoked, v.PolicyReason)
	}
}

// TestCanChatTrueForBuiltinServerWorkspace 内置服务端空间恒在线 → 可对话。
func TestCanChatTrueForBuiltinServerWorkspace(t *testing.T) {
	svc, _, _ := newCanChatEnv(t) // Hub 无该设备记录，也不得影响内置空间
	ctx := t.Context()

	v, err := svc.Get(ctx, store.BuiltinWorkspaceServerID)
	if err != nil {
		t.Fatalf("取内置空间失败: %v", err)
	}
	if !v.CanChat {
		t.Fatal("内置服务端空间恒在线，can_chat 必须为 true")
	}
	if !v.Online {
		t.Fatal("内置服务端空间恒在线，online 必须为 true")
	}
}
