package model

// Project 项目（主库一等实体）：工作区目录 + 默认控制权限 + 审批规则集合。
//
// 语义说明：
//   - DefaultAccessMode：项目级「默认控制权限」（confirm | full），仅在**新建会话那一刻**
//     被快照继承到会话的 AccessMode；之后改项目默认值不影响已建会话。
//   - Pinned：置顶（列表排序 pinned DESC, updated_at DESC）。
//   - DeletedAt：软删时间（缺省 = 未删除）。
//   - WorkspaceID：项目所属工作空间（执行面归属）；与 Path（磁盘受管目录）互相独立，
//     能力天花板由 Workspace.Kind 裁决（见 model.Workspace）。
type Project struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Path              string `json:"path"`
	DefaultAccessMode string `json:"default_access_mode"`
	Pinned            bool   `json:"pinned"`
	// WorkspaceID 所属工作空间；空串 = 未归属（历史数据回填后为 ws-server）。
	WorkspaceID string `json:"workspace_id,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	DeletedAt   *int64 `json:"deleted_at,omitempty"`
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
