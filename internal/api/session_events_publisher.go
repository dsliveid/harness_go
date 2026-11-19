// service.SessionChangePublisher → sessionbus.Bus 的适配器。
//
// # 为什么用适配器而不是让 sessionbus 直接实现该接口
//
// service 定义的是**它自己需要的窄接口**（consumer-side interface），
// 让 sessionbus 去实现它会把 sessionbus 的依赖方向拉到 service 上；
// 反之 service import sessionbus 同样是反向依赖。适配器放在两者之外
// （由装配层 cmd 引入）是唯一不破坏分层的位置。
//
// # 两道通道的分工
//
//	| 接口方法                        | 总线方法             | 是否合并 |
//	| ------------------------------- | -------------------- | -------- |
//	| PublishSessionUpdated           | PublishUpdated       | ✅ 1s    |
//	| PublishSessionStatusChanged     | PublishImmediate     | ❌ 立即  |
//
// 理由见 internal/sessionbus/bus.go 包注释：token 累加/消息时间戳是高频写，
// 而 Run 状态跃迁每次 Run 只有 2~3 次，不能忍受合并窗口的延迟。
package api

import "harness-go/internal/sessionbus"

// SessionBusPublisher 把会话摘要变更转发到列表级 SSE 总线。
type SessionBusPublisher struct {
	bus *sessionbus.Bus
}

// NewSessionBusPublisher 构造适配器。
//
// bus 为 nil 时返回 nil：装配层可无条件传，Publisher 为 nil 时
// service.summaryWriter 会走「纯回写」路径（不广播、不 panic）。
func NewSessionBusPublisher(bus *sessionbus.Bus) *SessionBusPublisher {
	if bus == nil {
		return nil
	}
	return &SessionBusPublisher{bus: bus}
}

// PublishSessionUpdated 会话摘要已变化（走合并窗口，吸收高频 token 累加）。
func (p *SessionBusPublisher) PublishSessionUpdated(sessionID string) {
	if p == nil || p.bus == nil {
		return
	}
	p.bus.PublishUpdated(sessionID)
}

// PublishSessionStatusChanged Run 状态跃迁（立即推，保证「运行中」徽标不滞后）。
func (p *SessionBusPublisher) PublishSessionStatusChanged(sessionID string) {
	if p == nil || p.bus == nil {
		return
	}
	p.bus.PublishImmediate(sessionID, sessionbus.EventUpdated)
}
