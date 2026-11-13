package toolapi

import (
	"context"
	"strings"
)

// CallInfo 一次工具调用在引擎侧的锚点信息（阶段 G）。
//
// 由 internal/tools.Runner 在调用插件前注入 ctx；快照存储（Host.Snapshots 的
// 内核实装）据此把快照与 session/run/tool_event 关联，回滚服务才能按消息/
// 轮次/工具级定位快照。插件本身不需要读它——只要把收到的 ctx 原样传给
// SnapshotStore.Create 即可完成关联。
type CallInfo struct {
	SessionID   string
	RunID       string
	ToolEventID string
}

type callInfoKey struct{}

// WithCallInfo 返回携带调用锚点的 ctx。
func WithCallInfo(ctx context.Context, info CallInfo) context.Context {
	return context.WithValue(ctx, callInfoKey{}, info)
}

// CallInfoFrom 从 ctx 取调用锚点；不存在时返回零值（如测试直调场景）。
func CallInfoFrom(ctx context.Context) CallInfo {
	if v, ok := ctx.Value(callInfoKey{}).(CallInfo); ok {
		return v
	}
	return CallInfo{}
}

// ============ 会话执行根（ctx 承载，接口签名零变更）============

// 为什么要用 ctx 而不是改 Workspace 接口：
//
//	Workspace 的最小二乘法是将 root 作为参数加进 Resolve/ReadFile/... 等 8 个方法，
//	但这会波及全部插件（fs/browser/memory/plan/skill/todo/webfetch…）与所有测试桩，
//	属于破坏性 ABI 变更。而工具调用链本来就已经在 invoke.go 注入 CallInfo(ctx)，
//	再塞一个执行根进去是零成本；插件代码一行都不用改 —— 它们本来就把收到的 ctx
//	原样透传给 host.Workspace().ReadFile(ctx, rel)。
//
// 🚨 铁律：ctx 根只表示「该会话的沙箱基准」，**不表示授权**。
// 载体（hostimpl）必须仍然拒绝绝对路径与 `..` 上跳，并在注入侧校验该根落在
// 服务端已开放的白名单内。缺任何一道，放开执行根就等于裸奔。
type workspaceRootKey struct{}

// WithWorkspaceRoot 返回携带「会话执行根」的 ctx（root 必须是已归一化的绝对路径）。
//
// 空 root 视为未指定：载体应回落进程级默认根，调用方可放心传空串。
func WithWorkspaceRoot(ctx context.Context, root string) context.Context {
	if strings.TrimSpace(root) == "" {
		return ctx
	}
	return context.WithValue(ctx, workspaceRootKey{}, root)
}

// WorkspaceRootFrom 从 ctx 取会话执行根；未注入时返回 ("", false)。
//
// 载体（如 hostimpl.workspace）必须把 false 当作「用进程级默认根」处理，
// 绝不可把空串当成根 —— 那会让 filepath.Join("", rel) 解析到进程 CWD。
func WorkspaceRootFrom(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(workspaceRootKey{}).(string)
	if !ok || strings.TrimSpace(v) == "" {
		return "", false
	}
	return v, true
}
