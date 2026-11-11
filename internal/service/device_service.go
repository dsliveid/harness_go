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
	// revokedHook 设备被吊销后的通知钩子（解绑双向收敛）。
	//
	// 为什么用后置注入而不是构造参数：service 不能 import internal/device 的
	// StateBus（依赖方向是 device → service 的观察回调，反过来会成环），
	// 与既有 SetDisconnector 同一手法。
	//
	// 用途：把「吊销」这件事广播到观察面（SSE device.revoked），
	// 否则管理端吊销后 Web 列表只能等用户手动点刷新才消失。
	revokedHook func(ctx context.Context, deviceID string)
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

// SetRevokedHook 注入「吊销后通知观察面」的钩子（cmd 装配时用：StateBus 建好后回填）。
//
// 与 SetDisconnector 同一取向后置注入：service 不 import device 包，
// 由装配方把 service.Revoke 与 StateBus.Remove 组合起来。
func (s *DeviceService) SetRevokedHook(fn func(ctx context.Context, deviceID string)) {
	s.revokedHook = fn
}

// notifyRevoked 吊销成功后的统一后置动作：断开长连接 + 广播观察面。
//
// 两件事都**不允许阻断**吊销本身：吊销已经落库生效，断连/广播失败只是
// 观察面滞后（SSE 会在下次重连快照时收敛），绝不能因此让 API 报错。
func (s *DeviceService) notifyRevoked(ctx context.Context, deviceID string) {
	if s.disconnector != nil {
		s.disconnector.CloseDevice(deviceID, "device.revoked")
	}
	if s.revokedHook != nil {
		s.revokedHook(ctx, deviceID)
	}
}

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
//
// userID 是授权者（签发配对码时的登录账号）。它由**需鉴权**的 handler 传入，
// 是「配对成功后免二次登录」的唯一身份来源：兑换时据此签发对应用户会话。
// 传空串合法（不携带授权身份），此时端侧配对后仍需登录。
func (s *DeviceService) IssuePairCode(ctx context.Context, userID, deviceName string, kind model.DeviceKind) (*PairCodeView, error) {
	if !s.enabled {
		return nil, apperr.New(apperr.CodeDeviceDisabled, "设备配对功能未启用")
	}
	pc, err := s.pairCodes.Issue(ctx, deviceName, kind, userID)
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

	// OwnerUserID 配对码携带的授权用户（生成配对码时的登录账号）。
	//
	// 刻意 `json:"-"`：它是**服务端内部凭据**，供 api 层据此顺带签发用户会话，
	// 绝不随配对响应裸奔（客户端需要的是签发结果 user_token，不是"谁的 id"）。
	// 为空表示本次配对不携带授权身份（历史码 / 未登录签发），端侧回落登录页。
	OwnerUserID string `json:"-"`
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
		// 归属落库：这是「设备与账号持久绑定」的写入点。
		// 配对码已被消费，此后设备归属只能从这里读；为空则是历史/无授权者配对，
		// 语义见 model.Device.UserID 的注释（/devices/session 会对它返回 owner_unknown）。
		UserID: strings.TrimSpace(pc.UserID),
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
	// 自动为一个执行面客户端建工作空间（协议 §2.4.4）。
	//
	// 只对 desktop / mobile 建：这两种形态在 WorkspaceKind 中有等价取值；
	// tablet / cli / browser 无对应语义，需要时手工创建。
	//
	// 幂等：按 device_id 先查后建。失败只记日志不阻断配对——配对是主链路，
	// 工作空间缺失可用手工补建兜底，不能让用户卡在"配不上对"。
	s.ensureWorkspaceForDevice(ctx, d)
	return &PairResult{
		DeviceID:        d.ID,
		DeviceToken:     plain,
		WSPath:          DeviceWSPath,
		ProtocolVersion: d.ProtocolVersion,
		OwnerUserID:     pc.UserID,
	}, nil
}

// ensureWorkspaceForDevice 配对后为设备创建对应工作空间（幂等，失败不阻断）。
func (s *DeviceService) ensureWorkspaceForDevice(ctx context.Context, d *model.Device) {
	if d == nil || s.main == nil {
		return
	}
	var kind model.WorkspaceKind
	switch d.Kind {
	case model.DeviceKindMobile:
		kind = model.WorkspaceKindMobile
	case model.DeviceKindDesktop:
		kind = model.WorkspaceKindDesktop
	default:
		// tablet / cli / browser：无对应 WorkspaceKind，不自动创建（协议 §2.4.4）。
		return
	}
	// 幂等：已有则复用，绝不重复建。
	if existing, err := s.main.Workspaces().GetByDeviceID(ctx, d.ID); err == nil && existing != nil {
		return
	}
	now := nowMillis()
	ws := &model.Workspace{
		ID:        "ws-" + d.ID,
		Name:      d.Name,
		Kind:      kind,
		DeviceID:  d.ID,
		RootPath:  "",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.main.Workspaces().Create(ctx, ws); err != nil {
		// 并发配对可能撞主键：再查一次确认确实存在，否则仅记日志。
		if _, getErr := s.main.Workspaces().GetByDeviceID(ctx, d.ID); getErr != nil {
			logDeviceWarn("配对后自动创建工作空间失败", d.ID, err)
		}
	}
}

// ============ 设备列表与吊销 ============

// List 列出设备（默认只含**未吊销**，按配对时间倒序填充能力项）。
//
// includeRevoked=true 时包含已吊销设备（审计/运维用）。
//
// 为什么默认排除已吊销：设备的「已吊销」在业务上等同于「已解除绑定」——
// 管理端点了「解除绑定」、或端侧主动解约、或服务端吊销后，用户期望它从
// 设备列表里**消失**，而不是变成一张灰掉的红标卡片（那会被读成「解绑没生效」）。
// 绑定关系的真源是 devices 表的 revoked_at，列表只是它的投影。
//
// 令牌摘要永不出栈（model.Device 的 TokenHash 已标注 json:"-"）；此处再显式清空，
// 防止将来有人给它加回 JSON 标签导致泄露。
func (s *DeviceService) List(ctx context.Context, includeRevoked bool) ([]model.Device, error) {
	var (
		list []model.Device
		err  error
	)
	if includeRevoked {
		list, err = s.main.Devices().ListAll(ctx)
	} else {
		list, err = s.main.Devices().List(ctx)
	}
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

// Revoke 吊销设备：置 revoked（令牌立即失效）+ 主动断开其长连接 + 广播观察面。
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
	s.notifyRevoked(ctx, deviceID)
	return nil
}

// RevokeSelf 端侧自我解约：凭 device_token 吊销**自己**。
//
// 解决的问题（双向解绑的另一半）：端侧「解除本机绑定」过去只清本地凭据，
// 服务端 devices 记录原封不动 → 管理端仍显示这台设备、令牌仍然有效。
// 根因不是客户端忘了调 DELETE /devices/{did}，而是那个端点走用户令牌中间件，
// 而端侧解绑时手里只有 device_token（user_token 可能已过期 7 天）。
//
// 安全边界：
//   - **只认令牌，不接受 deviceID** —— 目标由令牌哈希反查，物理上无法删别人；
//   - 令牌无效/设备不存在/已吊销一律按幂等成功返回 nil（目标状态已达成），
//     只有令牌缺失才报 401（那是调用方的编程错误，不是"已经解绑"）。
func (s *DeviceService) RevokeSelf(ctx context.Context, plainToken string) error {
	if !s.enabled {
		return apperr.New(apperr.CodeDeviceDisabled, "设备配对功能未启用")
	}
	token := strings.TrimSpace(plainToken)
	if token == "" {
		return apperr.New(apperr.CodeUnauthorized, "缺少设备令牌")
	}
	id, affected, err := s.main.Devices().RevokeByTokenHash(ctx, device.HashDeviceToken(token), store.NowMillis())
	if err != nil {
		return err
	}
	if !affected {
		// 令牌不存在或早已吊销：目标状态（本机不再绑定）已达成。
		return nil
	}
	s.notifyRevoked(ctx, id)
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

// DeviceSessionGrant 凭设备令牌换取用户会话的结果（api 层据此签发令牌）。
//
// 只带"该给谁签发"的身份事实，**不含令牌本身** —— 签发动作留在 api 层，
// 让 service 不依赖 auth（与 PairResult.OwnerUserID 同一取向）。
type DeviceSessionGrant struct {
	// UserID 设备归属账号（devices.user_id）。空串不可能出现在本结构里：
	// IssueSessionForDevice 在归属为空时已返回 device.owner_unknown。
	UserID string
	// DeviceID 用于日志与审计（不对外暴露）。
	DeviceID string
}

// IssueSessionForDevice 凭设备令牌换取该设备归属账号的用户会话身份。
//
// 解决的问题：user_token 过期（默认 7 天）后，端侧只剩 device_token，而设备令牌
// 通不过 /api/** 的用户令牌中间件。有了持久归属，服务端就能回答"该给谁续一张新会话"，
// 从而让"绑定即免登录"从一次性授权变成持久关系。
//
// 安全性（这是本方法最需要被审视的地方）：
//   - 它把 device_token 从"只能连 WS 执行面"提升为"可换取业务会话"。这不是新增
//     攻击面而是补齐已有能力 —— 持有 device_token 本就等同于在宿主上获得执行权
//     （可收 task.dispatch 执行 fs.* / exec），相对而言换取业务会话不是质变。
//   - 缓解：签发的会话由 api 层统一取 kind=web_session / scope=write（不授 admin）；
//     设备被吊销后 GetByTokenHash 立即查不到，本方法随之失效。
//   - 归属为空一律拒绝，绝不猜一个默认账号。
func (s *DeviceService) IssueSessionForDevice(ctx context.Context, plainToken string) (*DeviceSessionGrant, error) {
	if !s.enabled {
		return nil, apperr.New(apperr.CodeDeviceDisabled, "设备配对功能未启用")
	}
	// Authenticate 已完成：令牌非空校验 + 未吊销校验 + 清空 TokenHash（不外泄）。
	d, err := s.Authenticate(ctx, plainToken)
	if err != nil {
		return nil, err
	}
	owner := strings.TrimSpace(d.UserID)
	if owner == "" {
		// 历史设备（0011 之前配对）：令牌有效但归属未知，无法签发会话。
		// 这是**可解释的**降级，不是内部错误 —— 端侧应引导重新配对。
		return nil, apperr.New(apperr.CodeDeviceOwnerUnknown,
			"该设备的归属账号未知，请重新配对后再试")
	}
	return &DeviceSessionGrant{UserID: owner, DeviceID: d.ID}, nil
}

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
