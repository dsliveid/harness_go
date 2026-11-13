package service

import (
	"testing"

	"harness-go/internal/agent"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// ============ 移动端工作空间 = 纯对话（2026-11 产品决策）============
//
// 产品决策：移动端只做纯对话，不提供任何工具能力。
//
// 历史口径是 `ToolPolicyReadonly`（可对话 + 只读）。收紧为 `None` 的原因是
// readonly 会误导用户：UI 说"可以只读"，但移动端根本没有目录浏览能力
// （不做 fs.list），实际每次读文件都失败，体验比直接不提供更差。
//
// 🚨 本文件同时锁死**不被误伤**的两条既有策略：
//   - R1b：内置服务端空间无项目 → readonly（纯对话可只读）
//   - 桌面端在线设备 → full
//
// 这两条如果被本次收紧顺带改掉，会造成大面积能力退化，且很难在只测移动端的
// 用例里发现。

// ① 移动端空间（绑移动设备）→ 零工具。
func TestMobileWorkspacePolicyIsNone(t *testing.T) {
	env := newPolicyEnv(t)
	env.addDevice(t, "dev-phone", model.DeviceKindMobile)
	env.addWorkspace(t, "ws-phone", model.WorkspaceKindMobile, "dev-phone")
	env.hub.online["dev-phone"] = true

	p, reason := env.policy(t, "ws-phone")
	if p != agent.ToolPolicyNone {
		t.Fatalf("移动端空间应为 none，得到 %s", p)
	}
	if reason != policyReasonMobile {
		t.Fatalf("文案应为 %q，得到 %q", policyReasonMobile, reason)
	}
}

// ② 关键回归：内置服务端空间无项目（R1b）仍是 readonly，不被移动端收紧误伤。
func TestBuiltinPureChatStillReadonly(t *testing.T) {
	env := newPolicyEnv(t)

	p, reason := env.policy(t, store.BuiltinWorkspaceServerID)
	if p != agent.ToolPolicyReadonly {
		t.Fatalf("R1b 纯对话应保持 readonly，得到 %s（reason=%q）", p, reason)
	}
	if reason != policyReasonPureChat {
		t.Fatalf("文案应为 %q，得到 %q", policyReasonPureChat, reason)
	}
}

// ③ 关键回归：桌面端在线设备空间仍为 full（工具全开）。
func TestDesktopWorkspaceStillFull(t *testing.T) {
	env := newPolicyEnv(t)
	env.addDevice(t, "dev-pc", model.DeviceKindDesktop)
	env.addWorkspace(t, "ws-pc", model.WorkspaceKindDesktop, "dev-pc")
	env.hub.online["dev-pc"] = true

	p, reason := env.policy(t, "ws-pc")
	if p != agent.ToolPolicyFull {
		t.Fatalf("桌面端在线空间应为 full，得到 %s（reason=%q）", p, reason)
	}
	if reason != "" {
		t.Fatalf("full 不应带拒绝文案，得到 %q", reason)
	}
}

// ④ 移动端空间的视图层策略也必须同步为 none（前端下拉/面板据此展示）。
//
// 视图与 RunService 是两套组装代码，只看一处会漏掉"下拉显示可只读、实际零工具"的错位。
func TestMobileWorkspaceViewPolicyIsNone(t *testing.T) {
	svc, main, ctx := newBindTestEnv(t)
	addBindDevice(t, main, ctx, "dev-phone", model.DeviceKindMobile)
	w, err := svc.Create(ctx, CreateWorkspaceInput{Name: "手机空间", Kind: "mobile", DeviceID: "dev-phone"})
	if err != nil {
		t.Fatalf("创建移动端空间失败: %v", err)
	}
	if w.ToolPolicy != string(agent.ToolPolicyNone) {
		t.Fatalf("移动端空间视图应为 none，得到 %q", w.ToolPolicy)
	}
	if w.PolicyReason != policyReasonMobile {
		t.Fatalf("视图文案应为 %q，得到 %q", policyReasonMobile, w.PolicyReason)
	}
}
