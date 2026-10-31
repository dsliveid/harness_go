package toolapi

import "context"

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
