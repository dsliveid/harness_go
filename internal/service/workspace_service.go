package service

import (
	"context"
	"errors"
	"strings"

	"harness-go/internal/agent"
	"harness-go/internal/apperr"
	"harness-go/internal/device"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// WorkspaceView 工作空间对外视图（= 持久化字段 + 运行时派生字段）。
//
// 派生字段只在 service 层组装、绝不落库（在线状态是设备域实时事实）：
//   - Online：内置服务端空间恒 true；否则取 device.Hub 的实时在线判定；
//   - Host：设备空间为 "device:<deviceID>"，服务端内置空间为 "server"。
//     取值口径沿用 docs/工作区宿主与目录选择对接约定.md。
//
// 设备联动字段（D1~D4，前端据此渲染「已连接 / 未绑定 / 已删除」三分组）：
//   - Bound：是否绑定了设备（内置服务端空间为 true，它是自洽的执行宿主）；
//   - DeviceMissing：绑定关系仍在，但设备记录已不存在（物理删除）；
//   - DeviceRevoked：设备已解绑（软删 revoked_at 置位）；
//   - ToolPolicy：**工具能力权威裁决**，前端不得自行推导（同一套规则只在一处实现）。
type WorkspaceView struct {
	model.Workspace
	Online bool   `json:"online"`
	Host   string `json:"host"`
	// Bound 是否有可用执行宿主（内置空间恒 true）。
	Bound bool `json:"bound"`
	// DeviceName / DeviceKind / DeviceStatus 归属设备快照（未绑定/已删除时为空串）。
	DeviceName   string `json:"device_name,omitempty"`
	DeviceKind   string `json:"device_kind,omitempty"`
	DeviceStatus string `json:"device_status,omitempty"`
	// DeviceMissing 绑定的设备记录已不存在（需重新绑定；与「离线」严格区分）。
	DeviceMissing bool `json:"device_missing"`
	// DeviceRevoked 设备已解绑（不可恢复，需重新配对）。
	DeviceRevoked bool `json:"device_revoked"`
	// LastSeenAt 设备最近活跃时间（毫秒；nil = 未知或未绑定）。
	LastSeenAt *int64 `json:"last_seen_at,omitempty"`
	// ToolPolicy 工具能力策略（full|readonly|none）：**权威值**，与 RunService 裁决同源。
	ToolPolicy string `json:"tool_policy"`
	// PolicyReason 策略降级原因（人类可读，前端可直接展示）。
	PolicyReason string `json:"policy_reason,omitempty"`
	// CanChat 永远为 true（v2：无可用宿主也允许对话，只是没有工具能力）。
	// 保留该字段是为了让前端不必自己推断「能不能发消息」。
	CanChat bool `json:"can_chat"`
}

// HubOnline 设备在线判定窄接口（避免 service 直接依赖 *device.Hub 的具体实现）。
//
// 之所以用窄接口：批次 A 只依赖「是否在线」一个能力，且 Hub 可能为 nil（未启用设备域）。
type HubOnline interface {
	Online(deviceID string) bool
}

// WorkspaceService 工作空间用例：CRUD + 在线/宿主派生字段组装。
//
// 🚨 本层**不做能力判定**（能否执行命令/写文件由批次 B 的 workspace 能力天花板入口裁决），
// 这里只负责实体与视图。
type WorkspaceService struct {
	main *store.MainStore
	// hub 可能为 nil（设备域未启用）：此时所有设备空间一律视为离线，且不得 panic。
	hub HubOnline
	// sessions 可选（可能为 nil）：仅用于级联删除后清理会话物理目录。
	// 为 nil 时级联仍会软删主库会话行，只是目录留待启动清理兜底（不阻断删除）。
	sessions *store.SessionStore
}

// NewWorkspaceService 创建工作空间用例。
func NewWorkspaceService(main *store.MainStore, hub HubOnline) *WorkspaceService {
	return &WorkspaceService{main: main, hub: hub}
}

// WithSessionStore 注入会话库（可选）：级联删除后据此清理 sessions/<sid> 物理目录。
//
// 之所以用可选注入而非改构造函数签名：调用点较多（cmd 装配 / 集成测试），
// 且「删目录」只是收尾优化——缺失时不阻断删除（孤儿由启动清理兜底）。
func (s *WorkspaceService) WithSessionStore(sessions *store.SessionStore) *WorkspaceService {
	s.sessions = sessions
	return s
}

// List 列出全部工作空间并组装派生字段（服务端内置空间排最前，由仓储排序保证）。
func (s *WorkspaceService) List(ctx context.Context) ([]WorkspaceView, error) {
	list, err := s.main.Workspaces().List(ctx)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeInternal, "列出工作空间失败")
	}
	out := make([]WorkspaceView, 0, len(list))
	for _, w := range list {
		out = append(out, s.view(ctx, w))
	}
	return out, nil
}

// Get 取单个工作空间视图。
func (s *WorkspaceService) Get(ctx context.Context, id string) (*WorkspaceView, error) {
	w, err := s.main.Workspaces().Get(ctx, id)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeNotFound, "工作空间不存在")
	}
	v := s.view(ctx, *w)
	return &v, nil
}

// CreateInput 新建工作空间入参。
type CreateWorkspaceInput struct {
	Name     string
	Kind     string
	DeviceID string
	RootPath string
}

// Create 新建工作空间。
//
//   - kind 必须合法（server|desktop|mobile|browser），否则 400，绝不静默降级；
//   - DeviceID 非空时必须对应一台已存在的设备，否则 400（防止悬空的执行面归属）；
//   - 同一台设备只允许存在一个工作空间（配对自动建空间的幂等前提）。
func (s *WorkspaceService) Create(ctx context.Context, in CreateWorkspaceInput) (*WorkspaceView, error) {
	kind := model.WorkspaceKind(in.Kind)
	if !kind.Valid() {
		return nil, invalidArg("kind 必须是 server|desktop|mobile|browser")
	}
	name := in.Name
	if name == "" {
		name = string(kind)
	}
	if in.DeviceID != "" {
		dev, err := s.main.Devices().Get(ctx, in.DeviceID)
		if err != nil {
			return nil, wrapStoreErr(err, apperr.CodeDeviceNotFound, "归属设备不存在")
		}
		if err := checkKindMatchesDevice(kind, dev); err != nil {
			return nil, err
		}
		if existing, err := s.main.Workspaces().GetByDeviceID(ctx, in.DeviceID); err == nil && existing != nil {
			return nil, apperr.New(apperr.CodeConflict, "设备 %s 已有工作空间 %s", in.DeviceID, existing.ID)
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, wrapStoreErr(err, apperr.CodeInternal, "校验设备工作空间失败")
		}
	}
	w := &model.Workspace{
		ID:       "ws-" + newID(),
		Name:     name,
		Kind:     kind,
		DeviceID: in.DeviceID,
		RootPath: in.RootPath,
	}
	if err := s.main.Workspaces().Create(ctx, w); err != nil {
		return nil, wrapStoreErr(err, apperr.CodeInternal, "创建工作空间失败")
	}
	v := s.view(ctx, *w)
	return &v, nil
}

// UpdateWorkspaceInput 更新入参（零值表示沿用原值，便于 PATCH 语义）。
type UpdateWorkspaceInput struct {
	Name     *string
	DeviceID *string
	RootPath *string
	// Kind 仅用于**校验**：传了合法值也必须与原值一致，否则拒绝（改 kind = 提权）。
	Kind *string
}

// Update 更新名称 / 归属设备 / 根目录。
//
// 🚨 铁律一：kind 不可修改。哪怕传入与原值相同的 kind 也允许（幂等 PATCH），
// 但只要与库中不一致立即 400——kind 是权限天花板依据（docs/…协议.md §3）。
//
// 🚨 铁律二：内置服务端空间（ws-server）的 device_id **既不可绑定也不可解绑**。
// 它是「服务端本地执行面」的锚点：一旦绑上设备，executionDeviceID 会把工具派发到
// 该设备，BuiltinWorkspaceServerID「恒在服务端执行」的语义立即崩塌。
//
// 🚨 铁律三：**解绑不校验设备**（`device_id` 传空串时跳过设备存在性/在线/吊销判定）。
// 原因：设备被吊销或物理删除后，空间仍指向那台设备，若不放开解绑，这个空间
// 既删不掉（已绑定）又解不开（设备不存在）—— 变成永久的僵尸空间。
func (s *WorkspaceService) Update(ctx context.Context, id string, in UpdateWorkspaceInput) (*WorkspaceView, error) {
	cur, err := s.main.Workspaces().Get(ctx, id)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeNotFound, "工作空间不存在")
	}
	if in.Kind != nil && *in.Kind != "" && model.WorkspaceKind(*in.Kind) != cur.Kind {
		return nil, invalidArg("kind 不可修改（能力天花板依据，改 kind 等于提权）")
	}
	// 内置空间的 device_id 是只读锚点：任何变更请求（含置空）都拒绝。
	if id == store.BuiltinWorkspaceServerID && in.DeviceID != nil && *in.DeviceID != cur.DeviceID {
		return nil, invalidArg("内置服务端空间的执行面恒为服务端本身，不可绑定或解绑设备")
	}
	name := cur.Name
	if in.Name != nil {
		name = *in.Name
	}
	deviceID := cur.DeviceID
	if in.DeviceID != nil {
		deviceID = strings.TrimSpace(*in.DeviceID)
		// 只有「绑定到另一台设备」才需要校验；置空是解绑，必须无条件放行（铁律三）。
		if deviceID != "" && deviceID != cur.DeviceID {
			dev, err := s.main.Devices().Get(ctx, deviceID)
			if err != nil {
				return nil, wrapStoreErr(err, apperr.CodeDeviceNotFound, "归属设备不存在")
			}
			if err := checkKindMatchesDevice(cur.Kind, dev); err != nil {
				return nil, err
			}
			if existing, err := s.main.Workspaces().GetByDeviceID(ctx, deviceID); err == nil && existing != nil && existing.ID != id {
				return nil, apperr.New(apperr.CodeConflict, "设备 %s 已有工作空间 %s", deviceID, existing.ID)
			} else if err != nil && !errors.Is(err, store.ErrNotFound) {
				return nil, wrapStoreErr(err, apperr.CodeInternal, "校验设备工作空间失败")
			}
		}
	}
	rootPath := cur.RootPath
	if in.RootPath != nil {
		rootPath = *in.RootPath
	}
	if err := s.main.Workspaces().Update(ctx, id, name, deviceID, rootPath); err != nil {
		return nil, wrapStoreErr(err, apperr.CodeNotFound, "工作空间不存在")
	}
	return s.Get(ctx, id)
}

// Delete 删除工作空间。
//
// 错误映射：
//   - 不存在 → 404；
//   - 仍被会话/项目引用 → 409（store.ErrWorkspaceInUse，严格模式不级联）；
//   - 内置服务端空间 → 409（它是回填兜底，不可删）。
//
// 参数 migrateRefs：
//   - false = 严格模式（旧行为）：有会话/项目引用就 409，要求调用方先迁移；
//   - true  = 级联删除：单事务内把归属该空间的会话与项目一并软删、清掉其项目审批
//     规则，再物理删除空间行；事务成功后清理会话物理目录。
//     这是 Web 管理端「确认移除」的语义——用户要的是"删掉就真的没了"，
//     而不是"删了刷新又回来"。
//
// 🚨 绝不可改为「迁到内置 ws-server」：那是 ToolPolicyFull（可写 + 命令），
// 而原空间可能是 mobile（readonly）或设备离线（none），迁过去即静默提权（违反 D8）。
func (s *WorkspaceService) Delete(ctx context.Context, id string, migrateRefs bool) error {
	if !migrateRefs {
		err := s.main.Workspaces().Delete(ctx, id)
		return s.mapDeleteErr(err, id)
	}
	sessionIDs, err := s.main.Workspaces().DeleteMigratingRefs(ctx, id)
	if err != nil {
		return s.mapDeleteErr(err, id)
	}
	// 主库事务已提交：逐个清理会话物理目录。
	// 失败不回报错——会话行已软删（列表不可见），目录残留由启动清理兜底，
	// 与 SessionService.Delete 的既有约定一致（"行已删除，目录孤儿待清理"）。
	if s.sessions != nil {
		for _, sid := range sessionIDs {
			_ = s.sessions.DeleteSessionDir(sid)
		}
	}
	return nil
}

// mapDeleteErr 把 store 层删除错误映射为业务错误码（严格/级联两种模式共用）。
func (s *WorkspaceService) mapDeleteErr(err error, id string) error {
	if err == nil {
		return nil
	}
	// 先判「仍绑定设备」：它比「被引用」更需要用户做明确动作（解绑），
	// 且 migrate_refs 无法绕过——文案必须指向解绑，不能承诺迁移。
	if errors.Is(err, store.ErrWorkspaceBound) {
		return apperr.New(apperr.CodeConflict, "该工作空间仍绑定设备，请先解绑归属后再删除")
	}
	if errors.Is(err, store.ErrWorkspaceInUse) {
		return apperr.New(apperr.CodeConflict, "工作空间仍被会话或项目引用，请先迁移引用")
	}
	if errors.Is(err, store.ErrNotFound) {
		return wrapStoreErr(err, apperr.CodeNotFound, "工作空间不存在")
	}
	if id == store.BuiltinWorkspaceServerID {
		return apperr.New(apperr.CodeConflict, "内置服务端空间不可删除")
	}
	return wrapStoreErr(err, apperr.CodeInternal, "删除工作空间失败")
}

// view 组装派生字段（在线状态 / 设备快照 / 工具策略）。
//
// nil Hub（设备域未启用）时按离线处理，绝不 panic：能力判定会因离线而收窄，属失败即关闭。
func (s *WorkspaceService) view(ctx context.Context, w model.Workspace) WorkspaceView {
	v := WorkspaceView{
		Workspace: w,
		Host:      "server",
		Bound:     true,
		CanChat:   true,
		ToolPolicy: string(agent.ToolPolicyFull),
	}
	// 内置服务端空间：它自身就是执行宿主，恒在线。
	if w.ID == store.BuiltinWorkspaceServerID {
		if w.Kind == model.WorkspaceKindMobile {
			v.ToolPolicy = string(agent.ToolPolicyReadonly)
			v.PolicyReason = policyReasonMobile
		}
		return v
	}
	// 非内置空间必须有设备宿主。
	deviceID := strings.TrimSpace(w.DeviceID)
	if deviceID == "" {
		v.Host = "server"
		v.Online = false
		v.Bound = false
		v.ToolPolicy = string(agent.ToolPolicyNone)
		v.PolicyReason = policyReasonUnbound
		return v
	}
	v.Host = "device:" + deviceID
	v.Online = s.hub != nil && s.hub.Online(deviceID)

	// 设备快照：区分「记录已删除」与「已解绑」——两者都不可恢复，但文案不同。
	dev, err := s.main.Devices().Get(ctx, deviceID)
	switch {
	case err != nil:
		v.DeviceMissing = true
		v.Online = false
		v.Bound = false
		v.ToolPolicy = string(agent.ToolPolicyNone)
		v.PolicyReason = policyReasonDeviceMissing
		return v
	case dev.RevokedAt != nil:
		v.DeviceName = dev.Name
		v.DeviceKind = string(dev.Kind)
		v.DeviceStatus = string(dev.Status)
		v.Online = false
		v.Bound = false
		v.DeviceRevoked = true
		v.ToolPolicy = string(agent.ToolPolicyNone)
		v.PolicyReason = policyReasonDeviceRevoked
		return v
	}
	v.DeviceName = dev.Name
	v.DeviceKind = string(dev.Kind)
	v.DeviceStatus = string(dev.Status)
	v.LastSeenAt = dev.LastSeenAt

	// 设备存在且未解绑，但当前不在线 → 零工具（D3：绝不降级服务端本地执行）。
	if !v.Online {
		v.ToolPolicy = string(agent.ToolPolicyNone)
		v.PolicyReason = policyReasonOffline
		return v
	}
	if w.Kind == model.WorkspaceKindMobile {
		v.ToolPolicy = string(agent.ToolPolicyReadonly)
		v.PolicyReason = policyReasonMobile
	}
	return v
}

// 策略降级原因文案（唯一来源，RunService 与视图共用，保证提示一致）。
const (
	policyReasonUnbound       = "当前工作空间未绑定设备，无法调用工具"
	policyReasonDeviceMissing = "当前工作空间绑定的设备已删除，请重新绑定设备后再使用工具"
	policyReasonDeviceRevoked = "当前工作空间绑定的设备已解绑，请重新绑定设备后再使用工具"
	policyReasonOffline       = "当前工作空间的设备不在线，无法调用工具（设备恢复在线后自动可用）"
	policyReasonMobile        = "当前为移动端，仅支持只读操作与对话"
	policyReasonNoWorkspace   = "当前会话未绑定执行空间，仅支持只读操作与对话"
	// policyReasonPureChat 纯对话（绑服务端空间但无项目）——R1b。
	// 与 NoWorkspace 的区别：那条是「没有归属」（历史/异常态），本条是「有意为之的纯对话」。
	policyReasonPureChat = "纯对话不支持调用工具，请在绑定了设备的项目中开启会话来使用工具"
)

// checkKindMatchesDevice 校验工作空间 kind 与归属设备形态一致（D5）。
//
// 为什么必须校验：Workspace.Kind 是**能力天花板依据**（决定能否执行命令/写文件），
// 而 Device.Kind 是设备真实形态。把 mobile 空间绑到 desktop 设备上，等于用
// 「移动端禁写」的天花板去限制一台能力完备的桌面机（能力被白白丢弃）；
// 反之把 desktop 空间绑到 mobile 设备上，则会让移动端获得写权限（**提权漏洞**）。
//
// 只校验两者都有明确对应关系的情形：desktop↔desktop、mobile↔mobile、
// browser↔browser。tablet / cli 在 WorkspaceKind 里无对应取值（协议 §11.1 已定案
// 不做映射），因此不参与校验——它们本就无法自动生成工作空间。
func checkKindMatchesDevice(kind model.WorkspaceKind, dev *model.Device) error {
	if dev == nil {
		return invalidArg("归属设备不存在")
	}
	want, ok := workspaceKindForDevice(dev.Kind)
	if !ok {
		// 无对应关系的设备形态：不阻断（保持既有行为，协议未规定禁止手工绑定）。
		return nil
	}
	if kind != want {
		return invalidArg("工作空间类型 %s 与设备类型 %s 不匹配（设备 %s 需要 %s 空间）",
			kind, dev.Kind, dev.ID, want)
	}
	return nil
}

// workspaceKindForDevice 设备形态 → 对应的工作空间类型（无对应关系返回 ok=false）。
//
// 🚨 这张表必须与 device_service.ensureWorkspaceForDevice 的 switch 保持一致，
// 否则「配对自动建空间」与「手工改绑校验」会给出互相矛盾的结论。
func workspaceKindForDevice(k model.DeviceKind) (model.WorkspaceKind, bool) {
	switch k {
	case model.DeviceKindDesktop:
		return model.WorkspaceKindDesktop, true
	case model.DeviceKindMobile:
		return model.WorkspaceKindMobile, true
	case model.DeviceKindBrowser:
		return model.WorkspaceKindBrowser, true
	default:
		return "", false
	}
}

// 保持 device 包被引用（Hub 实现 HubOnline 的编译期断言见下方）。
var _ HubOnline = (*device.Hub)(nil)
