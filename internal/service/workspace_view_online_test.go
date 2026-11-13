package service

import (
	"testing"

	"harness-go/internal/model"
	"harness-go/internal/store"
)

// ============ 工作空间视图 online 派生字段（前端顶栏「在线/离线」的唯一依据）============
//
// 回归锁定（2026-11 实测缺陷）：服务端明明在运行、`GET /api/v1/workspaces` 也正常返回，
// 但前端顶栏显示「离线」、侧栏弹出「该工作空间离线，仅可查看历史对话」。
// 根因：`WorkspaceService.view()` 用值类型字面量初始化 WorkspaceView，
// bool 零值即 false；内置空间分支的注释写着「恒在线」却漏了 `v.Online = true`。
//
// 🚨 这组断言的意义：online 是**前端唯一**的在线判据，一旦为 false，
// 用户会以为服务端挂了（而实际上执行面完好），属于最容易被误判为「服务端不可用」的缺陷。

// TestBuiltinWorkspaceOnlineEvenWithoutHub 设备域未装配（hub=nil）时，内置空间仍必须在线。
//
// 内置空间的执行宿主就是服务端自身，与 Hub 的设备连接状态毫无关系。
func TestBuiltinWorkspaceOnlineEvenWithoutHub(t *testing.T) {
	svc, _, ctx := newBindTestEnv(t) // hub = nil
	v, err := svc.Get(ctx, store.BuiltinWorkspaceServerID)
	if err != nil {
		t.Fatalf("取内置空间失败: %v", err)
	}
	if !v.Online {
		t.Fatal("内置服务端空间必须恒在线（online=true），实际为 false —— 前端会误显离线")
	}
	if !v.Bound || !v.CanChat {
		t.Fatalf("内置空间应 bound=true 且 can_chat=true，得到 bound=%v can_chat=%v", v.Bound, v.CanChat)
	}
	if v.ToolPolicy != "full" {
		t.Fatalf("内置空间（kind=server）应为 full 工具策略，得到 %q", v.ToolPolicy)
	}
}

// TestBuiltinWorkspaceOnlineInList 列表接口里内置空间同样必须在线（前端消费的就是 List）。
func TestBuiltinWorkspaceOnlineInList(t *testing.T) {
	svc, main, ctx := newBindTestEnv(t)
	addBindDevice(t, main, ctx, "dev-x", model.DeviceKindDesktop)
	if _, err := svc.Create(ctx, CreateWorkspaceInput{Name: "桌面空间", Kind: "desktop", DeviceID: "dev-x"}); err != nil {
		t.Fatalf("创建工作空间失败: %v", err)
	}
	list, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("列出工作空间失败: %v", err)
	}
	var found bool
	for _, w := range list {
		if w.ID != store.BuiltinWorkspaceServerID {
			continue
		}
		found = true
		if !w.Online {
			t.Fatal("List 返回的内置空间 online 必须为 true")
		}
	}
	if !found {
		t.Fatal("List 结果必须包含内置服务端空间")
	}
}

// TestDeviceWorkspaceOfflineWhenHubMissing 对照组：无 Hub 时**设备空间**必须判离线。
//
// 这条断言防止「为了让内置空间在线」而把 online 一律写死 true：
// 设备空间的在线状态是真实运行时事实，不能伪造。
func TestDeviceWorkspaceOfflineWhenHubMissing(t *testing.T) {
	svc, main, ctx := newBindTestEnv(t) // hub = nil
	addBindDevice(t, main, ctx, "dev-20", model.DeviceKindDesktop)
	w, err := svc.Create(ctx, CreateWorkspaceInput{Name: "W", Kind: "desktop", DeviceID: "dev-20"})
	if err != nil {
		t.Fatalf("创建工作空间失败: %v", err)
	}
	if w.Online {
		t.Fatal("无 Hub 时设备空间必须判为离线，不得伪造在线")
	}
	if w.ToolPolicy != "none" {
		t.Fatalf("离线设备空间应为 none 工具策略，得到 %q", w.ToolPolicy)
	}
}
