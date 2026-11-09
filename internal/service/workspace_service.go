package service

import (
	"context"
	"errors"

	"harness-go/internal/apperr"
	"harness-go/internal/device"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// WorkspaceView 工作空间对外视图（= 持久化字段 + 运行时派生字段）。
//
// 派生字段只在 service 层组装、绝不落库（在线状态是设备域实时事实）：
//   - Online：DeviceID 为空（服务端内置空间）恒 true；否则取 device.Hub 的实时在线判定；
//   - Host：设备空间为 "device:<deviceID>"，服务端内置空间为 "server"。
//     取值口径沿用 docs/工作区宿主与目录选择对接约定.md。
type WorkspaceView struct {
	model.Workspace
	Online bool   `json:"online"`
	Host   string `json:"host"`
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
}

// NewWorkspaceService 创建工作空间用例。
func NewWorkspaceService(main *store.MainStore, hub HubOnline) *WorkspaceService {
	return &WorkspaceService{main: main, hub: hub}
}

// List 列出全部工作空间并组装派生字段（服务端内置空间排最前，由仓储排序保证）。
func (s *WorkspaceService) List(ctx context.Context) ([]WorkspaceView, error) {
	list, err := s.main.Workspaces().List(ctx)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeInternal, "列出工作空间失败")
	}
	out := make([]WorkspaceView, 0, len(list))
	for _, w := range list {
		out = append(out, s.view(w))
	}
	return out, nil
}

// Get 取单个工作空间视图。
func (s *WorkspaceService) Get(ctx context.Context, id string) (*WorkspaceView, error) {
	w, err := s.main.Workspaces().Get(ctx, id)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeNotFound, "工作空间不存在")
	}
	v := s.view(*w)
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
		if _, err := s.main.Devices().Get(ctx, in.DeviceID); err != nil {
			return nil, wrapStoreErr(err, apperr.CodeDeviceNotFound, "归属设备不存在")
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
	v := s.view(*w)
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
// 🚨 铁律：kind 不可修改。哪怕传入与原值相同的 kind 也允许（幂等 PATCH），
// 但只要与库中不一致立即 400——kind 是权限天花板依据（docs/…协议.md §3）。
func (s *WorkspaceService) Update(ctx context.Context, id string, in UpdateWorkspaceInput) (*WorkspaceView, error) {
	cur, err := s.main.Workspaces().Get(ctx, id)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeNotFound, "工作空间不存在")
	}
	if in.Kind != nil && *in.Kind != "" && model.WorkspaceKind(*in.Kind) != cur.Kind {
		return nil, invalidArg("kind 不可修改（能力天花板依据，改 kind 等于提权）")
	}
	name := cur.Name
	if in.Name != nil {
		name = *in.Name
	}
	deviceID := cur.DeviceID
	if in.DeviceID != nil {
		deviceID = *in.DeviceID
		if deviceID != "" && deviceID != cur.DeviceID {
			if _, err := s.main.Devices().Get(ctx, deviceID); err != nil {
				return nil, wrapStoreErr(err, apperr.CodeDeviceNotFound, "归属设备不存在")
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
//   - 仍被会话/项目引用 → 409（store.ErrWorkspaceInUse，不做级联）；
//   - 内置服务端空间 → 409（它是回填兜底，不可删）。
func (s *WorkspaceService) Delete(ctx context.Context, id string) error {
	err := s.main.Workspaces().Delete(ctx, id)
	if err == nil {
		return nil
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

// view 组装派生字段。
//
// nil Hub（设备域未启用）时按离线处理，绝不 panic：能力判定会因离线而收窄，属失败即关闭。
func (s *WorkspaceService) view(w model.Workspace) WorkspaceView {
	v := WorkspaceView{Workspace: w, Host: "server", Online: true}
	if w.DeviceID == "" {
		return v
	}
	v.Host = "device:" + w.DeviceID
	v.Online = s.hub != nil && s.hub.Online(w.DeviceID)
	return v
}

// 保持 device 包被引用（Hub 实现 HubOnline 的编译期断言见下方）。
var _ HubOnline = (*device.Hub)(nil)
