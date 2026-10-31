package device

import (
	"encoding/json"
	"fmt"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/model"
)

// ============ 设备协议：信封 + 消息类型 + 版本协商（阶段 C-3） ============
//
// 设计铁律（docs/protocols.md §6 / 开发实施计划 v2.x「协议向后兼容」）：
//   - 所有帧共用**同一个信封** Envelope{v, type, id, ts, payload}，形状在 A 阶段冻结
//   - 后续只允许**新增字段**，禁止删除/改名；新增消息类型只能追加常量
//   - 端侧永不直写 DB（不变量 1）：本文件只做形状定义与校验，不含任何 IO

// 协议版本协商：服务端支持 N 与 N-1（N 为当前版本）。
const (
	// CurrentProtocolVersion 服务端当前设备协议版本（N）。
	CurrentProtocolVersion = model.DefaultDeviceProtocolVersion
	// MinProtocolVersion 服务端仍接受的最低版本。
	//
	// N=1 时最低版本即 1；将来升到 2 时这里保持 1（双代兼容一个版本），
	// 升到 3 时才随之上移到 2 —— 收敛节奏与 observ.ProtocolVersion 同步。
	MinProtocolVersion = 1
)

// MessageType 设备协议消息类型（字符串枚举，便于日志检索与前端直接消费）。
type MessageType string

// 消息类型常量（端→服 / 服→端）。
const (
	// MsgHello 端→服：连上后的首帧自述（可选；缺省时以配对本机记录为准）。
	MsgHello MessageType = "hello"
	// MsgWelcome 服→端：握手确认（携带服务端时间、心跳参数、协议版本）。
	MsgWelcome MessageType = "welcome"
	// MsgPing 端→服：每 15s 一次，携带负载/能力摘要/协议版本。
	MsgPing MessageType = "ping"
	// MsgPong 服→端：对 ping 的应答，携带服务端时间与待派发任务数。
	MsgPong MessageType = "pong"
	// MsgError 双向：错误帧（Fatal=true 表示随后断开）。
	MsgError MessageType = "error"
	// MsgTaskDispatch 服→端：任务派发（C-4 dispatcher 发出）。
	MsgTaskDispatch MessageType = "task.dispatch"
	// MsgTaskProgress 端→服：执行进度流式块（C-4）。
	MsgTaskProgress MessageType = "task.progress"
	// MsgTaskResult 端→服：任务终态结果（C-4）。
	MsgTaskResult MessageType = "task.result"
	// MsgLeaseRevoked 服→端：会话租约被其他端抢占（C-6）。
	MsgLeaseRevoked MessageType = "lease.revoked"
)

// 帧级错误码（信封 error.payload.code，与 apperr 的对外错误码区分：
// 这里是协议内部码，端侧按它决定是否重连/升级）。
const (
	// FrameCodeBadFrame 帧格式非法（可继续使用连接）。
	FrameCodeBadFrame = "device.bad_frame"
	// FrameCodeProtocolMismatch 协议版本不兼容（Fatal，端侧应提示升级）。
	FrameCodeProtocolMismatch = "device.protocol_mismatch"
	// FrameCodeTaskUnsupported 服务端未启用远端执行（C-4 之前）。
	FrameCodeTaskUnsupported = "device.task_unsupported"
	// FrameCodeReplaced 同一设备被新连接替换（旧连接被服务端关闭）。
	FrameCodeReplaced = "device.replaced"
	// FrameCodeOfflineTimeout 超时无帧，服务端判定离线并关闭。
	FrameCodeOfflineTimeout = "device.offline_timeout"
	// FrameCodeRevoked 设备已被吊销。
	FrameCodeRevoked = "device.revoked"
	// FrameCodeServerClosing 服务端关闭（进程退出/设备域停用）。
	FrameCodeServerClosing = "device.server_closing"
	// FrameCodeWriteFailed 服务端写失败（通常是端侧已断开）。
	FrameCodeWriteFailed = "device.write_failed"
)

// IsTaskFrame 是否为任务派发族的帧（由 C-4 dispatcher 消费）。
func (t MessageType) IsTaskFrame() bool {
	switch t {
	case MsgTaskDispatch, MsgTaskProgress, MsgTaskResult:
		return true
	}
	return false
}

// Valid 是否为已知消息类型。
func (t MessageType) Valid() bool {
	switch t {
	case MsgHello, MsgWelcome, MsgPing, MsgPong, MsgError,
		MsgTaskDispatch, MsgTaskProgress, MsgTaskResult, MsgLeaseRevoked:
		return true
	}
	return false
}

// Envelope 统一帧信封（所有设备协议消息的唯一外形）。
type Envelope struct {
	// V 协议版本（缺省 0 视为「未声明」，按最低兼容版本处理）。
	V int `json:"v"`
	// Type 消息类型。
	Type MessageType `json:"type"`
	// ID 帧 ID（请求/应答配对与幂等用；任务族复用 tool_event_id）。
	ID string `json:"id,omitempty"`
	// RunID 关联 Run（任务族与租约族使用；其余为空）。
	RunID string `json:"run_id,omitempty"`
	// TS 发送端毫秒时间戳（仅观测用，不参与判定）。
	TS int64 `json:"ts,omitempty"`
	// Payload 具体负载（原始 JSON，按 Type 二次解码）。
	Payload json.RawMessage `json:"payload,omitempty"`
}

// NewEnvelope 构造带当前版本号与时间戳的信封（payload 可为 nil）。
func NewEnvelope(t MessageType, id string, payload any) (Envelope, error) {
	return newEnvelopeAt(t, id, payload, time.Now())
}

// newEnvelopeAt 以指定时间构造（测试注入时钟用，避免 time.Now 抖动）。
func newEnvelopeAt(t MessageType, id string, payload any, now time.Time) (Envelope, error) {
	env := Envelope{V: CurrentProtocolVersion, Type: t, ID: id, TS: now.UnixMilli()}
	if payload == nil {
		return env, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("device: 序列化 %s 帧负载失败: %w", t, err)
	}
	env.Payload = raw
	return env, nil
}

// MustEnvelope 构造信封，失败返回带错误帧的兜底信封（仅内部已知结构体使用）。
func MustEnvelope(t MessageType, id string, payload any) Envelope {
	env, err := NewEnvelope(t, id, payload)
	if err != nil {
		return Envelope{V: CurrentProtocolVersion, Type: MsgError, ID: id}
	}
	return env
}

// Decode 把 Payload 解码到 v（空负载时置零并返回 nil）。
func (e Envelope) Decode(v any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(e.Payload, v); err != nil {
		return fmt.Errorf("device: 解析 %s 帧负载失败: %w", e.Type, err)
	}
	return nil
}

// Validate 校验帧基本合法性（类型必须已知；轮次版本由 CompatibleVersion 判定）。
func (e Envelope) Validate() error {
	if e.Type == "" {
		return fmt.Errorf("device: 帧缺少 type")
	}
	if !e.Type.Valid() {
		return fmt.Errorf("device: 未知帧类型 %q", e.Type)
	}
	return nil
}

// SupportedVersions 返回服务端接受的版本集合（N 与 N-1）。
func SupportedVersions() []int {
	cur := CurrentProtocolVersion
	if MinProtocolVersion >= cur {
		return []int{cur}
	}
	return []int{cur, cur - 1}
}

// CompatibleVersion 判定端侧声明的协议版本是否兼容。
//
// 语义（宽进严出）：
//   - v == 0：端侧未声明（老端/首帧省略）→ 按最低兼容版本接受
//   - MinProtocolVersion ≤ v ≤ CurrentProtocolVersion → 兼容
//   - 其余（过旧/超前）→ device.protocol_mismatch，端侧必须提示升级，**不得静默降级执行**
func CompatibleVersion(v int) error {
	if v == 0 {
		return nil
	}
	if v >= MinProtocolVersion && v <= CurrentProtocolVersion {
		return nil
	}
	return apperr.New(apperr.CodeDeviceProtocolMismatch,
		"端侧协议版本 %d 不受支持（服务端支持 %d~%d）", v, MinProtocolVersion, CurrentProtocolVersion)
}

// ============ 各帧负载（新增字段只能追加，禁止改语义） ============

// HelloPayload 端侧自述（首帧）。
type HelloPayload struct {
	Name             string             `json:"name,omitempty"`
	Platform         string             `json:"platform,omitempty"`
	AppVersion       string             `json:"app_version,omitempty"`
	CapabilitiesHash string             `json:"capabilities_hash,omitempty"`
	ProtocolVersion  int                `json:"protocol_version,omitempty"`
	Capabilities     []model.Capability `json:"capabilities,omitempty"`
}

// WelcomePayload 握手确认（服务端参数下发，端侧据此驱动心跳节奏）。
type WelcomePayload struct {
	DeviceID        string `json:"device_id"`
	ServerTime      int64  `json:"server_time"`
	ProtocolVersion int    `json:"protocol_version"`
	HeartbeatS      int    `json:"heartbeat_s"`
	OfflineAfterS   int    `json:"offline_after_s"`
	Pending         int    `json:"pending"`
	// LeaseEpoch 会话租约代次（C-6 填充；0 表示无租约信息）。
	LeaseEpoch int64 `json:"lease_epoch,omitempty"`
}

// PingPayload 端侧心跳（每 15s）。
type PingPayload struct {
	Load             json.RawMessage `json:"load,omitempty"`
	CapabilitiesHash string          `json:"capabilities_hash,omitempty"`
	ProtocolVersion  int             `json:"protocol_version,omitempty"`
}

// PongPayload 服务端心跳应答。
type PongPayload struct {
	ServerTime      int64 `json:"server_time"`
	Pending         int   `json:"pending"`
	ProtocolVersion int   `json:"protocol_version"`
}

// ErrorPayload 错误帧负载。
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
	// Fatal 为 true 时服务端在写出本帧后关闭连接。
	Fatal bool `json:"fatal,omitempty"`
}

// TaskDispatchPayload 任务派发（C-4；形状在 A 阶段已冻结）。
type TaskDispatchPayload struct {
	TaskID      string          `json:"task_id"` // = tool_event_id，端侧据此做幂等
	RunID       string          `json:"run_id"`
	ToolEventID string          `json:"tool_event_id"`
	ToolName    string          `json:"tool_name"`
	Args        json.RawMessage `json:"args,omitempty"`
	Deadline    int64           `json:"deadline"`          // 毫秒时间戳，超时端侧可放弃
	Attempt     int             `json:"attempt,omitempty"` // 重派次数（>=1）
}

// TaskProgressPayload 执行进度（C-4；端侧只产事件，不落库）。
type TaskProgressPayload struct {
	TaskID      string `json:"task_id"`
	RunID       string `json:"run_id"`
	ToolEventID string `json:"tool_event_id"`
	Chunk       string `json:"chunk"`
}

// TaskResultPayload 任务终态（C-4）。端侧结果一律 source_trust=untrusted。
type TaskResultPayload struct {
	TaskID      string          `json:"task_id"`
	RunID       string          `json:"run_id"`
	ToolEventID string          `json:"tool_event_id"`
	Status      string          `json:"status"` // success | failed | cancelled
	Result      json.RawMessage `json:"result,omitempty"`
	Error       string          `json:"error,omitempty"`
	SourceTrust string          `json:"source_trust,omitempty"`
}

// LeaseRevokedPayload 租约被抢占通知（C-6）。
type LeaseRevokedPayload struct {
	SessionID string `json:"session_id"`
	Epoch     int64  `json:"epoch"`
	Reason    string `json:"reason,omitempty"`
}
