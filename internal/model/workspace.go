package model

// WorkspaceKind 工作空间类型（= 能力天花板依据）。
//
// 🚨 取值必须与 web/src/types.ts 的 Workspace.kind 逐字一致（server|desktop|mobile|browser），
// 不额外引入 cli / tablet：Device.Kind 与 WorkspaceKind 是两套独立枚举，
// 不做映射（docs/工作空间模型与三端能力收口协议.md §11.1 已定案）。
type WorkspaceKind string

const (
	// WorkspaceKindServer 服务端内置空间（唯一无需设备即可执行的空间）。
	WorkspaceKindServer WorkspaceKind = "server"
	// WorkspaceKindDesktop 桌面端宿主空间（具备完整命令执行/文件写入能力）。
	WorkspaceKindDesktop WorkspaceKind = "desktop"
	// WorkspaceKindMobile 移动端宿主空间（仅对话 + 图片识别，禁止命令执行与文件写入）。
	WorkspaceKindMobile WorkspaceKind = "mobile"
	// WorkspaceKindBrowser 浏览器宿主空间（能力受浏览器沙箱限制）。
	WorkspaceKindBrowser WorkspaceKind = "browser"
)

// Valid 判断 kind 是否为合法取值（新增取值必须同步前端与能力判定表）。
func (k WorkspaceKind) Valid() bool {
	switch k {
	case WorkspaceKindServer, WorkspaceKindDesktop, WorkspaceKindMobile, WorkspaceKindBrowser:
		return true
	default:
		return false
	}
}

// Workspace 工作空间（主库一等实体）：会话与项目的执行面归属。
//
// 语义说明：
//   - Kind：能力天花板依据（server|desktop|mobile|browser），**创建后不可修改**
//     （改 kind = 提权，服务层直接拒绝）。
//   - DeviceID 空 = 服务端内置空间（ws-server），恒在线；非空 = 归属某台已配对设备，
//     在线状态由设备域 Hub 实时派生（Online 属 service 层派生字段，不落库）。
//   - RootPath：宿主侧根目录；服务端内置空间为空串（会话各自的 workspace_path 才是磁盘路径）。
type Workspace struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Kind      WorkspaceKind `json:"kind"`
	DeviceID  string        `json:"device_id"`
	RootPath  string        `json:"root_path"`
	CreatedAt int64         `json:"created_at"`
	UpdatedAt int64         `json:"updated_at"`
}
