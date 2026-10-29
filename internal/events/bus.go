// Package events 实现 per-Run 事件总线：环形缓冲 + 多订阅 + 背压处理。
//
// 核心设计（docs/架构设计文档.md §3.4）：
//   - 每个 Run 一个环形缓冲（默认 500 条），用于 Last-Event-ID 断线补发
//   - 每个订阅者独立 channel（默认 256 缓冲）
//   - **慢消费者绝不阻塞 Run**：缓冲满即主动断开该订阅，由客户端重连走 resync
//   - delta 事件不落库，只有里程碑事件（消息完成/tool.started/result/审批/状态）落库
package events

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// ProtocolVersion 事件协议版本（多端双代兼容的判据）。
const ProtocolVersion = 1

// Type 事件类型。
type Type string

const (
	// Run 生命周期
	TypeRunStatus Type = "run.status" // payload: {status, stop_reason, error_code}

	// 消息
	TypeMessageStart Type = "message.start" // payload: {message_id, role, seq}
	TypeMessageDelta Type = "message.delta" // payload: {message_id, delta}（不落库，仅预览）
	TypeMessageEnd   Type = "message.end"   // payload: {message_id, content, token_count}

	// 工具
	TypeToolStarted  Type = "tool.started"  // payload: {tool_event_id, tool_name, args, risk}
	TypeToolProgress Type = "tool.progress" // payload: {tool_event_id, chunk}
	TypeToolResult   Type = "tool.result"   // payload: {tool_event_id, status, result, truncated}

	// 审批
	TypeApprovalRequest  Type = "approval.request"  // payload: {approval_id, tool_event_id, tool_name, risk}
	TypeApprovalDecision Type = "approval.decision" // payload: {approval_id, decision, reason}

	// 重连对齐（服务端在补发完成后发出，携带当前消息快照摘要）
	TypeResync Type = "resync" // payload: {last_seq, message_count, run_status}

	// 错误（非致命，Run 仍可继续）
	TypeError Type = "error" // payload: {code, message}
)

// Event 一条 Run 事件。
type Event struct {
	V       int             `json:"v"`      // 协议版本
	RunID   string          `json:"run_id"` // 归属 Run
	Seq     int64           `json:"seq"`    // Run 内单调递增（从 1 开始）
	Type    Type            `json:"type"`
	At      int64           `json:"at"` // Unix 毫秒
	Payload json.RawMessage `json:"payload,omitempty"`
}

// NewEvent 构造事件（payload 序列化为 JSON；nil 表示无 payload）。
func NewEvent(runID string, seq int64, typ Type, payload any) (Event, error) {
	ev := Event{V: ProtocolVersion, RunID: runID, Seq: seq, Type: typ, At: time.Now().UnixMilli()}
	if payload == nil {
		return ev, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("events: 序列化 payload 失败: %w", err)
	}
	ev.Payload = raw
	return ev, nil
}

// CloseReason 订阅关闭原因。
type CloseReason string

const (
	// ReasonSlowConsumer 订阅者消费过慢，缓冲已满 → 主动断开（不阻塞 Run）
	ReasonSlowConsumer CloseReason = "slow_consumer"
	// ReasonRunClosed Run 结束，事件源关闭
	ReasonRunClosed CloseReason = "run_closed"
	// ReasonBusClosed 总线关闭（进程退出）
	ReasonBusClosed CloseReason = "bus_closed"
)

// Subscriber 事件订阅者。
type Subscriber struct {
	ID    int64
	RunID string

	ch     chan Event
	once   sync.Once
	mu     sync.Mutex
	reason CloseReason
}

// C 返回事件通道。通道关闭表示订阅已结束（原因见 CloseReason）。
func (s *Subscriber) C() <-chan Event { return s.ch }

// CloseReason 返回关闭原因（未关闭时为空）。
func (s *Subscriber) CloseReason() CloseReason {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

// close 幂等关闭（关闭 channel 并记录原因）。
func (s *Subscriber) close(reason CloseReason) {
	s.once.Do(func() {
		s.mu.Lock()
		s.reason = reason
		s.mu.Unlock()
		close(s.ch)
	})
}

// trySend 非阻塞发送；缓冲满返回 false（调用方据此断开慢消费者）。
func (s *Subscriber) trySend(ev Event) bool {
	s.mu.Lock()
	closed := s.reason != ""
	s.mu.Unlock()
	if closed {
		return false
	}
	select {
	case s.ch <- ev:
		return true
	default:
		return false
	}
}

// runLog 单个 Run 的事件环形缓冲与订阅集合。
type runLog struct {
	events  []Event // 最近 ringSize 条
	nextSeq int64
	subs    map[int64]*Subscriber
	closed  bool
}

// Bus per-Run 事件总线。
type Bus struct {
	mu       sync.Mutex
	ringSize int
	subBuf   int
	nextSub  int64
	runs     map[string]*runLog
}

// NewBus 创建总线。ringSize<=0 用 500，subBuf<=0 用 256。
func NewBus(ringSize, subBuf int) *Bus {
	if ringSize <= 0 {
		ringSize = 500
	}
	if subBuf <= 0 {
		subBuf = 256
	}
	return &Bus{ringSize: ringSize, subBuf: subBuf, runs: make(map[string]*runLog)}
}

// Publish 发布事件（分配 seq 并广播给所有订阅者）。
//
// 慢消费者处理：缓冲满即断开该订阅，**绝不阻塞发布方（即不阻塞 Run）**。
func (b *Bus) Publish(runID string, typ Type, payload any) (Event, error) {
	if runID == "" {
		return Event{}, fmt.Errorf("events: runID 不能为空")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	rl, ok := b.runs[runID]
	if !ok {
		rl = &runLog{subs: make(map[int64]*Subscriber)}
		b.runs[runID] = rl
	}
	if rl.closed {
		return Event{}, fmt.Errorf("events: Run %s 的事件源已关闭", runID)
	}

	rl.nextSeq++
	ev, err := NewEvent(runID, rl.nextSeq, typ, payload)
	if err != nil {
		rl.nextSeq--
		return Event{}, err
	}

	// 写入环形缓冲
	rl.events = append(rl.events, ev)
	if len(rl.events) > b.ringSize {
		rl.events = rl.events[len(rl.events)-b.ringSize:]
	}

	// 广播（非阻塞）
	for id, sub := range rl.subs {
		if !sub.trySend(ev) {
			sub.close(ReasonSlowConsumer)
			delete(rl.subs, id)
		}
	}
	return ev, nil
}

// Subscribe 订阅 Run 事件。
//
// fromSeq 语义：>=0 时先补发 seq > fromSeq 的历史事件（对应 SSE 的 Last-Event-ID 续传，
// **fromSeq=0 表示从头补发**，保证订阅晚于 Run 完成的客户端仍能拿到完整输出）；
// 传 -1 表示只要新事件、不要历史。
// 历史补发与加入订阅在同一把锁内完成，保证不漏事件。
func (b *Bus) Subscribe(runID string, fromSeq int64) (*Subscriber, error) {
	if runID == "" {
		return nil, fmt.Errorf("events: runID 不能为空")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	rl, ok := b.runs[runID]
	if !ok {
		rl = &runLog{subs: make(map[int64]*Subscriber)}
		b.runs[runID] = rl
	}

	b.nextSub++
	sub := &Subscriber{
		ID:    b.nextSub,
		RunID: runID,
		ch:    make(chan Event, b.subBuf),
	}

	// 先补发历史（ring 内仍保留的部分）
	if fromSeq >= 0 {
		for _, ev := range rl.events {
			if ev.Seq <= fromSeq {
				continue
			}
			select {
			case sub.ch <- ev:
			default:
				// 历史补发就溢出（订阅者缓冲太小），直接判定为慢消费者
				sub.close(ReasonSlowConsumer)
				return sub, nil
			}
		}
	}

	// Run 已结束：不发新事件，立即告知调用方（补发完历史后关闭）
	if rl.closed {
		sub.close(ReasonRunClosed)
		return sub, nil
	}

	rl.subs[sub.ID] = sub
	return sub, nil
}

// Unsubscribe 取消订阅（幂等）。
func (b *Bus) Unsubscribe(sub *Subscriber) {
	if sub == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if rl, ok := b.runs[sub.RunID]; ok {
		delete(rl.subs, sub.ID)
	}
	sub.close(ReasonRunClosed)
}

// CloseRun 关闭某个 Run 的事件源（Run 进入终态后调用）。
// 已订阅者收到通道关闭通知；历史 ring 保留，便于客户端随后补发对齐。
func (b *Bus) CloseRun(runID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rl, ok := b.runs[runID]
	if !ok {
		return
	}
	rl.closed = true
	for id, sub := range rl.subs {
		sub.close(ReasonRunClosed)
		delete(rl.subs, id)
	}
}

// Close 关闭总线（进程退出）。
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for runID, rl := range b.runs {
		rl.closed = true
		for id, sub := range rl.subs {
			sub.close(ReasonBusClosed)
			delete(rl.subs, id)
		}
		delete(b.runs, runID)
	}
}

// SubscriberCount 某 Run 的当前订阅者数量（测试与监控用）。
func (b *Bus) SubscriberCount(runID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if rl, ok := b.runs[runID]; ok {
		return len(rl.subs)
	}
	return 0
}

// LastSeq 某 Run 已发布的最大 seq（0 表示无事件）。
func (b *Bus) LastSeq(runID string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if rl, ok := b.runs[runID]; ok {
		return rl.nextSeq
	}
	return 0
}
