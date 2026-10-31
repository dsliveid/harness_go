package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/device"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// DeviceWSPath 设备长连接（出站 WS）端点路径，配对成功时下发给端侧。
const DeviceWSPath = "/api/v1/devices/ws"

// devicePairAttemptLimit 同一来源的**配对失败**次数上限（防暴力枚举 6 位码）。
//
// 只统计失败：成功不影响后续配对；成功后即清零，避免正常联调被误伤。
const devicePairAttemptLimit = 6

// DeviceServiceOptions DeviceService 依赖与配置（由 cmd 从 config.Devices 段映射）。
type DeviceServiceOptions struct {
	Main *store.MainStore

	Enabled        bool          // 设备域总开关（false 时端点返回 device.disabled）
	PairTTL        time.Duration // 配对码有效期，默认 120s
	Heartbeat      time.Duration // 端侧 ping 间隔
	OfflineAfter   time.Duration // 无帧判离线阈值
	TaskTTL        time.Duration // 任务派发超时
	StrictApproval bool          // 高风险审批是否仅限执行端设备确认

	// Disconnector 吊销设备时主动断开其长连接（C-3 网关实现；未接线为 nil）。
	Disconnector DeviceDisconnector

	// Leases 会话租约管理器（C-6）。可为 nil：设备域关闭时引擎不做跨端租约，
	// 仅保留本地 mutex（行为与 C 阶段之前完全一致）。
	Leases *device.LeaseManager
}

// DeviceDisconnector 由 WS 网关注入的断连能力（窄接口，避免 service 依赖 gateway 细节）。
type DeviceDisconnector interface {
	CloseDevice(deviceID, reason string)
}

// DeviceService 设备用例（阶段 C-2）：配对、列表、吊销。
//
// 铁律：
//   - 设备表全在主库 → 与配对码消费同库，天然无跨库事务
//   - 令牌明文**永不出栈**（List/Get 一律清空 TokenHash，仅配对成功返回一次明文）
//   - 配对失败统一 apperr.CodeDevicePairCodeInvalid（不存在/已消费/已过期同码，防枚举）
type DeviceService struct {
	main          *store.MainStore
	pairCodes     *device.PairCodeIssuer
	enabled       bool
	heartbeat     time.Duration
	offlineAfter  time.Duration
	taskTTL       time.Duration
	strictApprove bool
	disconnector  DeviceDisconnector
	leases        *device.LeaseManager
	limiter       *attemptLimiter
}

// NewDeviceService 创建设备用例；会话/维护任务通过其方法访问配对与租约信息。
func NewDeviceService(opts DeviceServiceOptions) *DeviceService {
	ttl := opts.PairTTL
	if ttl <= 0 {
		ttl = device.DefaultPairTTL
	}
	heartbeat := opts.Heartbeat
	if heartbeat <= 0 {
		heartbeat = 15 * time.Second
	}
	offline := opts.OfflineAfter
	if offline <= 0 {
		offline = 3 * heartbeat
	}
	taskTTL := opts.TaskTTL
	if taskTTL <= 0 {
		taskTTL = 600 * time.Second
	}
	return &DeviceService{
		main:          opts.Main,
		pairCodes:     device.NewPairCodeIssuer(opts.Main.PairCodes(), ttl),
		enabled:       opts.Enabled,
		heartbeat:     heartbeat,
		offlineAfter:  offline,
		taskTTL:       taskTTL,
		strictApprove: opts.StrictApproval,
		disconnector:  opts.Disconnector,
		leases:        opts.Leases,
		limiter:       newAttemptLimiter(devicePairAttemptLimit, time.Minute),
	}
}

// Enabled 设备域是否启用。
func (s *DeviceService) Enabled() bool { return s.enabled }

// PairTTL 配对码有效期（端点回显用）。
func (s *DeviceService) PairTTL() time.Duration { return s.pairCodes.TTL() }

// Heartbeat 心跳间隔（C-3 网关与前端提示用）。
func (s *DeviceService) Heartbeat() time.Duration { return s.heartbeat }

// OfflineAfter 无帧判离线阈值。
func (s *DeviceService) OfflineAfter() time.Duration { return s.offlineAfter }

// TaskTTL 任务派发超时。
func (s *DeviceService) TaskTTL() time.Duration { return s.taskTTL }

// StrictApprovalDevice 高风险审批是否仅限执行端设备确认。
func (s *DeviceService) StrictApprovalDevice() bool { return s.strictApprove }

// SetPairClock 注入时钟（测试用）。
func (s *DeviceService) SetPairClock(fn func() time.Time) { s.pairCodes.SetClock(fn) }

// SetDisconnector 注入断连能力（cmd 装配时用：WS 网关注册后回填）。
//
// 之所以后置注入：DeviceService 与 device.Gateway 相互引用（网关鉴权调 service），
// 用窄接口 + 后置 setter 打破构造期循环依赖。
func (s *DeviceService) SetDisconnector(d DeviceDisconnector) { s.disconnector = d }

// ============ 在线状态落库（C-3 网关回调，错误只记日志不阻断连接） ============
//
// 三者都是 Hub 的持久化钩子：网关/hub 只管在线状态，落库一律经这里；
// 同一主库、单条 UPDATE，天然无跨库事务。

// MarkOnline 标记设备上线（WS 注册成功时）。
func (s *DeviceService) MarkOnline(ctx context.Context, deviceID string) {
	if err := s.main.Devices().MarkOnline(ctx, deviceID, nowMillis()); err != nil {
		// 设备可能刚被吊销（终态），属正常竞态，不阻断
		logDeviceWarn("标记设备在线失败", deviceID, err)
	}
}

// MarkOffline 标记设备离线（断线或心跳超时）。
func (s *DeviceService) MarkOffline(ctx context.Context, deviceID string) {
	if err := s.main.Devices().MarkOffline(ctx, deviceID, nowMillis()); err != nil {
		logDeviceWarn("标记设备离线失败", deviceID, err)
	}
}

// RecordHeartbeat 刷新心跳与负载快照（ping / hello 帧）。
func (s *DeviceService) RecordHeartbeat(ctx context.Context, deviceID string, at int64,
	load json.RawMessage, capsHash string, proto int) {
	if at <= 0 {
		at = nowMillis()
	}
	if err := s.main.Devices().UpdateHeartbeat(ctx, deviceID, at, load, capsHash, proto); err != nil {
		logDeviceWarn("刷新设备心跳失败", deviceID, err)
	}
}

// nowMillis 当前毫秒时间戳。
func nowMillis() int64 { return time.Now().UnixMilli() }

// logDeviceWarn 设备域非致命错误日志（slog 默认 logger，避免把 logger 注入每处）。
func logDeviceWarn(msg, deviceID string, err error) {
	slog.Default().Warn(msg, "device_id", deviceID, "err", err)
}

// ============ 配对码 ============

// PairCodeView 配对码签发结果（管理端展示，TTL 120s）。
type PairCodeView struct {
	Code       string           `json:"code"`
	ExpiresAt  int64            `json:"expires_at"`
	TTLSec     int              `json:"ttl_s"`
	DeviceName string           `json:"device_name,omitempty"`
	DeviceKind model.DeviceKind `json:"device_kind,omitempty"`
}

// IssuePairCode 生成 6 位配对码（TTL 120s，一次性消费）。
func (s *DeviceService) IssuePairCode(ctx context.Context, deviceName string, kind model.DeviceKind) (*PairCodeView, error) {
	if !s.enabled {
		return nil, apperr.New(apperr.CodeDeviceDisabled, "设备配对功能未启用")
	}
	pc, err := s.pairCodes.Issue(ctx, deviceName, kind)
	if err != nil {
		return nil, err
	}
	return &PairCodeView{
		Code:       pc.Code,
		ExpiresAt:  pc.ExpiresAt,
		TTLSec:     int(s.pairCodes.TTL() / time.Second),
		DeviceName: pc.DeviceName,
		DeviceKind: pc.DeviceKind,
	}, nil
}

// PairRequest 端侧配对请求。
type PairRequest struct {
	Code         string             `json:"code"`
	Name         string             `json:"name"`
	Kind         model.DeviceKind   `json:"kind"`
	Platform     string             `json:"platform"`
	AppVersion   string             `json:"app_version"`
	Capabilities []model.Capability `json:"capabilities"`
}

// PairResult 配对成功结果：device_token 明文**仅此一次**返回。
type PairResult struct {
	DeviceID        string `json:"device_id"`
	DeviceToken     string `json:"device_token"`
	WSPath          string `json:"ws_path"`
	ProtocolVersion int    `json:"protocol_version"`
}

// Pair 端侧凭码配对：原子消费码 → 创建设备 → 落能力项 → 返回令牌明文。
//
// 失败一律返回 apperr.CodeDevicePairCodeInvalid（防枚举）；失败计入来源限流（6 次/分钟）。
func (s *DeviceService) Pair(ctx context.Context, clientKey string, req PairRequest) (*PairResult, error) {
	if !s.enabled {
		return nil, apperr.New(apperr.CodeDeviceDisabled, "设备配对功能未启用")
	}
	key := strings.TrimSpace(clientKey)
	if s.limiter.exceeded(key) {
		return nil, apperr.New(apperr.CodeRateLimited, "配对失败次数过多，请稍后再试")
	}

	pc, err := s.pairCodes.Redeem(ctx, req.Code)
	if err != nil {
		if errors.Is(err, device.ErrPairCodeInvalid) {
			s.limiter.fail(key)
			return nil, apperr.New(apperr.CodeDevicePairCodeInvalid, "配对码无效或已过期")
		}
		return nil, err
	}

	kind := req.Kind
	if !kind.Valid() {
		kind = pc.DeviceKind
	}
	if !kind.Valid() {
		kind = model.DeviceKindDesktop
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = pc.DeviceName
	}
	if name == "" {
		name = string(kind)
	}

	plain, err := device.NewDeviceToken()
	if err != nil {
		return nil, err
	}
	caps := normalizeCapabilities(req.Capabilities, "")
	d := &model.Device{
		ID:               newID(),
		Name:             name,
		Kind:             kind,
		Platform:         strings.TrimSpace(req.Platform),
		Status:           model.DeviceOffline, // 配对完成≠已连上，等 WS 上线再置 online
		CapabilitiesHash: device.CapabilitiesHash(caps),
		ProtocolVersion:  model.DefaultDeviceProtocolVersion,
		AppVersion:       strings.TrimSpace(req.AppVersion),
		TokenHash:        device.HashDeviceToken(plain),
	}
	if err := s.main.Devices().Create(ctx, d); err != nil {
		return nil, err
	}
	caps = normalizeCapabilities(req.Capabilities, d.ID)
	if err := s.main.Capabilities().Replace(ctx, d.ID, caps); err != nil {
		// 补偿删除：避免留下没有能力项的半成品设备（同一主库，无需跨库事务）
		_ = s.main.Devices().Delete(ctx, d.ID)
		return nil, err
	}
	s.limiter.reset(key)
	return &PairResult{
		DeviceID:        d.ID,
		DeviceToken:     plain,
		WSPath:          DeviceWSPath,
		ProtocolVersion: d.ProtocolVersion,
	}, nil
}

// ============ 设备列表与吊销 ============

// List 列出全部设备（含离线/已吊销），按配对时间倒序填充能力项。
//
// 令牌摘要永不出栈（model.Device 的 TokenHash 已标注 json:"-"）；此处再显式清空，
// 防止将来有人给它加回 JSON 标签导致泄露。
func (s *DeviceService) List(ctx context.Context) ([]model.Device, error) {
	list, err := s.main.Devices().List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].TokenHash = ""
		caps, err := s.main.Capabilities().ListByDevice(ctx, list[i].ID)
		if err != nil {
			return nil, err
		}
		list[i].Capabilities = caps
	}
	return list, nil
}

// Get 取单个设备（含能力项）。
func (s *DeviceService) Get(ctx context.Context, deviceID string) (*model.Device, error) {
	d, err := s.main.Devices().Get(ctx, deviceID)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeDeviceNotFound, "设备不存在")
	}
	d.TokenHash = ""
	if caps, err := s.main.Capabilities().ListByDevice(ctx, d.ID); err == nil {
		d.Capabilities = caps
	}
	return d, nil
}

// Revoke 吊销设备：置 revoked（令牌立即失效）+ 主动断开其长连接。
//
// 幂等：设备不存在返回 device.not_found；已吊销的设备重复吊销同样返回 not_found
// （仓储 Revoke 的条件是 revoked_at IS NULL）。
func (s *DeviceService) Revoke(ctx context.Context, deviceID string) error {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return invalidArg("设备 ID 不能为空")
	}
	if err := s.main.Devices().Revoke(ctx, deviceID, store.NowMillis()); err != nil {
		return wrapStoreErr(err, apperr.CodeDeviceNotFound, "设备不存在或已吊销")
	}
	if s.disconnector != nil {
		s.disconnector.CloseDevice(deviceID, "device.revoked")
	}
	return nil
}

// Authenticate 用设备令牌换取设备（WS 升级前鉴权；已吊销/不存在均返回 unauthorized）。
func (s *DeviceService) Authenticate(ctx context.Context, token string) (*model.Device, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, apperr.New(apperr.CodeUnauthorized, "缺少设备令牌")
	}
	d, err := s.main.Devices().GetByTokenHash(ctx, device.HashDeviceToken(token))
	if err != nil {
		// 不存在与已吊销同码（GetByTokenHash 已排除 revoked）→ 不可区分，防枚举
		return nil, wrapStoreErr(err, apperr.CodeUnauthorized, "设备令牌无效")
	}
	d.TokenHash = ""
	return d, nil
}

// Close 释放资源（配对码签发器无后台协程，当前为空实现，保持与其他用例一致的生命周期）。
func (s *DeviceService) Close() {}

// normalizeCapabilities 清洗能力项：丢弃空名、去重（同名保留首个）、填充 device_id。
func normalizeCapabilities(in []model.Capability, deviceID string) []model.Capability {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]model.Capability, 0, len(in))
	for _, c := range in {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, model.Capability{
			DeviceID: deviceID,
			Name:     name,
			Version:  strings.TrimSpace(c.Version),
		})
	}
	return out
}

// ============ 失败限流（进程内滑动窗口，仅用于配对入口） ============

// attemptLimiter 按来源键统计**失败**次数的滑动窗口限流器。
type attemptLimiter struct {
	limit  int
	window time.Duration
	mu     sync.Mutex
	now    func() time.Time
	hits   map[string][]time.Time
}

func newAttemptLimiter(limit int, window time.Duration) *attemptLimiter {
	if limit <= 0 {
		limit = devicePairAttemptLimit
	}
	if window <= 0 {
		window = time.Minute
	}
	return &attemptLimiter{limit: limit, window: window, now: time.Now, hits: make(map[string][]time.Time)}
}

// exceeded 判断该来源是否已达失败上限（只判定，不记录）。
func (l *attemptLimiter) exceeded(key string) bool {
	if key == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.keep(key)) >= l.limit
}

// fail 记录一次失败。
func (l *attemptLimiter) fail(key string) {
	if key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hits[key] = append(l.keep(key), l.now())
}

// reset 成功一次即清零（正常联调不被误伤）。
func (l *attemptLimiter) reset(key string) {
	if key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}

// keep 淘汰窗口外的历史命中（调用方必须持锁）。
func (l *attemptLimiter) keep(key string) []time.Time {
	cutoff := l.now().Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}
