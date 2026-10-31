package service

import (
	"context"
	"errors"
	"strings"

	"harness-go/internal/agent"
	"harness-go/internal/device"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// ============ 会话租约适配（阶段 C-6） ============
//
// 依赖倒置：agent 只声明窄接口 agent.LeaseManager（在 agent 包内定义），
// device.LeaseManager 满足同一方法集，本文件负责把仓储的 ErrNotFound
// 翻译成 device 包约定的「不存在返回 (nil, nil)」，避免让 device 包 import store。

// NewLeaseAdapter 把主库租约仓储适配成 device.LeaseStore，
// 并组装出可直接注入 agent.Deps.Leases 的租约管理器。
//
// hub 可为 nil（此时只维护 DB 租约、不推送 lease.revoked），由 cmd 装配时传入。
func NewLeaseAdapter(main *store.MainStore, hub device.ConnFinder) *device.LeaseManager {
	return device.NewLeaseManager(device.LeaseManagerOptions{
		Store: &leaseStoreAdapter{repo: main.Leases()},
		Hub:   hub,
	})
}

// leaseStoreAdapter 把 store.LeaseRepo 适配成 device.LeaseStore。
type leaseStoreAdapter struct{ repo *store.LeaseRepo }

func (a *leaseStoreAdapter) Acquire(ctx context.Context, sessionID, deviceID string,
	now, ttlMillis int64) (model.SessionLease, bool, error) {
	return a.repo.Acquire(ctx, sessionID, deviceID, now, ttlMillis)
}

// Get 不存在时返回 (nil, nil)：语义是「当前无写者」，不是错误。
func (a *leaseStoreAdapter) Get(ctx context.Context, sessionID string) (*model.SessionLease, error) {
	lease, err := a.repo.Get(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return lease, nil
}

func (a *leaseStoreAdapter) Release(ctx context.Context, sessionID, deviceID string) (bool, error) {
	return a.repo.Release(ctx, sessionID, deviceID)
}

func (a *leaseStoreAdapter) DeleteExpired(ctx context.Context, now int64) (int64, error) {
	return a.repo.DeleteExpired(ctx, now)
}

// 编译期断言：device.LeaseManager 满足 agent 的租约端口（改动签名会在这里炸开）。
var _ agent.LeaseManager = (*device.LeaseManager)(nil)

// LeaseView 会话租约的对外视图（GET /sessions/{sid}/lease）。
type LeaseView struct {
	// SessionID 会话 ID。
	SessionID string `json:"session_id"`
	// Holder 写者归属：设备 ID；空串表示服务端本地执行面。
	Holder string `json:"holder"`
	// HolderKind executor 语义（server | device），便于前端直接展示。
	HolderKind string `json:"holder_kind"`
	// Epoch 写者纪元（每次换人或过期接管 +1；同持有者续期不变）。
	Epoch int64 `json:"epoch"`
	// Active 当前是否仍有效（false 表示无写者或已过期）。
	Active bool `json:"active"`
	// ExpiresAt / HeartbeatAt 毫秒时间戳（0 = 无租约）。
	ExpiresAt   int64 `json:"expires_at"`
	HeartbeatAt int64 `json:"heartbeat_at"`
}

// SessionLease 查询某会话的租约归属（C-6）。无租约或已过期返回 Active=false 的视图（非错误）。
func (s *DeviceService) SessionLease(ctx context.Context, sessionID string, now int64) (*LeaseView, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, invalidArg("会话 ID 不能为空")
	}
	if s.leases == nil {
		// 未装配租约（设备域关闭）→ 视为服务端独占，Active=true / epoch=0
		return &LeaseView{
			SessionID:  sessionID,
			Holder:     "",
			HolderKind: string(model.ExecutorServer),
			Active:     true,
		}, nil
	}
	lease, err := s.leases.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	view := &LeaseView{SessionID: sessionID, HolderKind: string(model.ExecutorServer)}
	if lease == nil {
		// 无租约 = 服务端本地可写（引擎单进程由本地 mutex 串行），不改语义只做展示
		view.Active = true
		return view, nil
	}
	view.Holder = lease.DeviceID
	view.HolderKind = string(model.ExecutorServer)
	if lease.DeviceID != "" {
		view.HolderKind = string(model.ExecutorDevice)
	}
	view.Epoch = lease.Epoch
	view.ExpiresAt = lease.ExpiresAt
	view.HeartbeatAt = lease.HeartbeatAt
	view.Active = !lease.Expired(now)
	return view, nil
}

// Leases 返回租约管理器（cmd 装配时注入 agent.Deps.Leases；nil = 未启用）。
func (s *DeviceService) Leases() *device.LeaseManager { return s.leases }
