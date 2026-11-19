package model

// Project 项目（主库一等实体）：工作区目录 + 默认控制权限 + 审批规则集合。
//
// 语义说明：
//   - DefaultAccessMode：项目级「默认控制权限」（confirm | full），仅在**新建会话那一刻**
//     被快照继承到会话的 AccessMode；之后改项目默认值不影响已建会话。
//   - Pinned：置顶（列表排序 pinned DESC, updated_at DESC）。
//   - DeletedAt：软删时间（缺省 = 未删除）。
//   - WorkspaceID：项目所属**工作空间**（执行面归属）；与 Path（**项目目录**，磁盘受管目录）互相独立，
//     能力天花板由 Workspace.Kind 裁决（见 model.Workspace）。
//     术语「工作空间 vs 项目目录」的权威定义见 docs/术语表-工作空间与项目目录.md。
type Project struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Path              string `json:"path"`
	DefaultAccessMode string `json:"default_access_mode"`
	Pinned            bool   `json:"pinned"`
	// Constraints 项目约束正文（自由文本，注入到 prompt 的「## 项目约束」段）。
	// 0014 起落主库；此前只在前端 localStorage，刷新/换端即丢。
	Constraints string `json:"constraints"`
	// PlanMode 规划与执行策略（standard | always_plan | always_proceed）。
	// 0014 起落主库；空串按 standard 兜底（对存量老项目安全）。
	PlanMode string `json:"plan_mode"`
	// WorkspaceID 所属工作空间；空串 = 未归属（历史数据回填后为 ws-server）。
	WorkspaceID string `json:"workspace_id,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	DeletedAt   *int64 `json:"deleted_at,omitempty"`
}

// ProjectLink 项目设置里的「关联项目」：对**另一目录**的引用 + 说明。
//
// 只存路径与描述，不存对方项目 id —— 关联目标可能根本不是本系统里的项目。
type ProjectLink struct {
	ID          string `json:"id"`
	ProjectID   string `json:"project_id"`
	Path        string `json:"path"`
	Description string `json:"description"`
	CreatedAt   int64  `json:"created_at"`
}

// ProjectDataGrant 项目级「数据权限」策略：某个数据范围当前是拒绝/询问/允许。
//
// 🚨 语义关键：**表里没有行 = ask（每次询问）**，不预置任何默认行。
// 预置行会让「用户从未表态」与「用户明确选了 ask」不可区分，后续想调整默认值会被脏数据卡住。
type ProjectDataGrant struct {
	ProjectID string `json:"project_id"`
	Scope     string `json:"scope"`
	Mode      string `json:"mode"`
	UpdatedAt int64  `json:"updated_at"`
}

// ProjectApprovalRule 项目级审批规则：命中 (ToolName, Pattern) 的工具调用直接放行。
//
// Pattern 语义由下游下发引擎解释（阶段四 approval_policy），本层只负责持久化。
type ProjectApprovalRule struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	ToolName  string `json:"tool_name"`
	Pattern   string `json:"pattern"`
	CreatedAt int64  `json:"created_at"`
}
