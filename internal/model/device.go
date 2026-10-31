package model

import "encoding/json"

// DeviceKind 设备形态（多端协同 HALO）。
type DeviceKind string

const (
	DeviceKindDesktop DeviceKind = "desktop"
	DeviceKindTablet  DeviceKind = "tablet"
	DeviceKindMobile  DeviceKind = "mobile"
	DeviceKindCLI     DeviceKind = "cli"
	DeviceKindBrowser DeviceKind = "browser"
)

// Valid 是否为合法设备形态。
func (k DeviceKind) Valid() bool {
	switch k {
	case DeviceKindDesktop, DeviceKindTablet, DeviceKindMobile, DeviceKindCLI, DeviceKindBrowser:
		return true
	}
	return false
}

// DeviceStatus 设备在线状态（revoked 为终态，不可回到 online）。
type DeviceStatus string

const (
	DeviceOnline  DeviceStatus = "online"
	DeviceOffline DeviceStatus = "offline"
	DeviceRevoked DeviceStatus = "revoked"
)

// Valid 是否为合法设备状态。
func (s DeviceStatus) Valid() bool {
	switch s {
	case DeviceOnline, DeviceOffline, DeviceRevoked:
		return true
	}
	return false
}

// DefaultDeviceProtocolVersion 服务端当前设备协议版本（支持 N / N-1）。
const DefaultDeviceProtocolVersion = 1

// Device 已配对设备（主库 devices 表）。令牌明文永不入库，只存 sha256。
type Device struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Kind             DeviceKind      `json:"kind"`
	Platform         string          `json:"platform"`
	Status           DeviceStatus    `json:"status"`
	CapabilitiesHash string          `json:"capabilities_hash"`
	ProtocolVersion  int             `json:"protocol_version"`
	AppVersion       string          `json:"app_version"`
	LoadJSON         json.RawMessage `json:"load,omitempty"`
	TokenHash        string          `json:"-"`
	PairedAt         int64           `json:"paired_at"`
	LastSeenAt       *int64          `json:"last_seen_at,omitempty"`
	RevokedAt        *int64          `json:"revoked_at,omitempty"`
	CreatedAt        int64           `json:"created_at"`
	UpdatedAt        int64           `json:"updated_at"`

	// Capabilities 读路径按需填充（列表/详情用），不落 devices 表。
	Capabilities []Capability `json:"capabilities,omitempty"`
}

// Online 是否在线（已吊销视为离线）。
func (d *Device) Online() bool { return d.Status == DeviceOnline }

// Capability 设备能力项（capabilities 表，(device_id, name) 唯一）。
type Capability struct {
	DeviceID  string `json:"device_id,omitempty"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	UpdatedAt int64  `json:"updated_at"`
}

// DevicePairCode 一次性配对码（TTL 120s，落库以便服务重启不打断配对窗口）。
// ConsumedAt 非 nil 即已消费；已消费/过期/不存在三种情况上层返回同一错误（防枚举）。
type DevicePairCode struct {
	Code       string     `json:"code"`
	DeviceName string     `json:"device_name"`
	DeviceKind DeviceKind `json:"device_kind"`
	ExpiresAt  int64      `json:"expires_at"`
	ConsumedAt *int64     `json:"consumed_at,omitempty"`
	CreatedAt  int64      `json:"created_at"`
}

// Usable 在 now 时刻是否仍可用于配对。
func (p *DevicePairCode) Usable(now int64) bool {
	return p.ConsumedAt == nil && p.ExpiresAt > now
}

// SessionLease 会话写者租约（主库 session_leases 表）。
//
// 语义：session_id 唯一；每次抢占 device_id 换人且 epoch+1，旧端收 lease.revoked。
// DeviceID 为空串表示服务端本地执行面持有。
type SessionLease struct {
	SessionID   string `json:"session_id"`
	DeviceID    string `json:"device_id"`
	Epoch       int64  `json:"epoch"`
	ExpiresAt   int64  `json:"expires_at"`
	HeartbeatAt int64  `json:"heartbeat_at"`
	CreatedAt   int64  `json:"created_at"`
}

// Expired 在 now 时刻是否已过期。
func (l *SessionLease) Expired(now int64) bool { return l.ExpiresAt <= now }

// HeldBy 是否由 deviceID 持有（未过期才算持有）。
func (l *SessionLease) HeldBy(deviceID string, now int64) bool {
	return l.DeviceID == deviceID && !l.Expired(now)
}
