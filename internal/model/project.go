package model

// Project 项目（主库一等实体）：工作区目录 + 默认控制权限 + 审批规则集合。
//
// 语义说明：
//   - DefaultAccessMode：项目级「默认控制权限」（confirm | full），仅在**新建会话那一刻**
//     被快照继承到会话的 AccessMode；之后改项目默认值不影响已建会话。
//   - Pinned：置顶（列表排序 pinned DESC, updated_at DESC）。
//   - DeletedAt：软删时间（缺省 = 未删除）。
type Project struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Path              string `json:"path"`
	DefaultAccessMode string `json:"default_access_mode"`
	Pinned            bool   `json:"pinned"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
	DeletedAt         *int64 `json:"deleted_at,omitempty"`
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
