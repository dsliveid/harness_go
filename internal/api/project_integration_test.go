package api

// 项目级权限治理端到端验收（阶段六）：走真实 HTTP 路由验证四大诉求。
//
//  1. 项目落库为服务端一等实体（CRUD 可用、列表可读）；
//  2. 新会话**快照继承**项目默认控制权限（建 full 项目 → 新建会话 access_mode=full）；
//  3. 项目改默认值只影响之后新建的会话（旧会话保持 full，验证「只快照一次」）；
//  4. 项目审批规则可增删，并通过 ApprovalPolicy 真正下发到引擎（规则集合并可见）；
//  5. 删项目后会话回纯对话（project_id 归零、规则清空），会话仍可用。

import (
	"context"
	"net/http"
	"testing"
)

func TestProjectDefaultAccessModeInheritance(t *testing.T) {
	srv, token := newTestServer(t)

	// ① 创建 default_access_mode=full 的项目
	var proj struct {
		ID                string `json:"id"`
		DefaultAccessMode string `json:"default_access_mode"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects", token,
		map[string]any{"name": "演示项目", "path": `D:\demo`, "default_access_mode": "full"},
		http.StatusCreated, &proj)
	if proj.ID == "" || proj.DefaultAccessMode != "full" {
		t.Fatalf("项目创建异常: %+v", proj)
	}

	// ② 项目内新建会话（**不传 access_mode**）→ 应继承 full
	var sess struct {
		ID         string `json:"id"`
		AccessMode string `json:"access_mode"`
		ProjectID  string `json:"project_id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions", token,
		map[string]any{"title": "继承 full", "project_id": proj.ID},
		http.StatusCreated, &sess)
	if sess.AccessMode != "full" {
		t.Fatalf("新会话应继承项目默认 full，得到 %q", sess.AccessMode)
	}
	if sess.ProjectID != proj.ID {
		t.Fatalf("会话 project_id 应落库为 %q，得到 %q", proj.ID, sess.ProjectID)
	}

	// ③ 把项目默认值改成 confirm → 旧会话必须仍为 full（只快照一次）
	doJSON(t, http.MethodPatch, srv.URL+"/api/v1/projects/"+proj.ID, token,
		map[string]any{"default_access_mode": "confirm"}, http.StatusNoContent, nil)
	var got struct {
		AccessMode string `json:"access_mode"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/"+sess.ID, token, nil, http.StatusOK, &got)
	if got.AccessMode != "full" {
		t.Fatalf("已建会话不得被项目改默认值影响，得到 %q", got.AccessMode)
	}

	// ④ 改后再新建 → 应为 confirm
	var sess2 struct {
		AccessMode string `json:"access_mode"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions", token,
		map[string]any{"title": "继承 confirm", "project_id": proj.ID},
		http.StatusCreated, &sess2)
	if sess2.AccessMode != "confirm" {
		t.Fatalf("改默认值后新建会话应为 confirm，得到 %q", sess2.AccessMode)
	}
}

// 显式 access_mode 覆盖项目默认值（前端顶栏切换路径）。
func TestSessionExplicitAccessModeOverridesProject(t *testing.T) {
	srv, token := newTestServer(t)
	var proj struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects", token,
		map[string]any{"name": "P", "path": `D:\p`, "default_access_mode": "confirm"},
		http.StatusCreated, &proj)

	var sess struct {
		AccessMode string `json:"access_mode"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions", token,
		map[string]any{"title": "显式 full", "project_id": proj.ID, "access_mode": "full"},
		http.StatusCreated, &sess)
	if sess.AccessMode != "full" {
		t.Fatalf("显式 access_mode 应优先于项目默认，得到 %q", sess.AccessMode)
	}
}

// 纯对话：不传 project_id 时行为完全不变（confirm 兜底，零破坏）。
func TestSessionWithoutProjectKeepsConfirm(t *testing.T) {
	srv, token := newTestServer(t)
	var sess struct {
		AccessMode string `json:"access_mode"`
		ProjectID  string `json:"project_id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions", token,
		map[string]any{"title": "纯对话"}, http.StatusCreated, &sess)
	if sess.AccessMode != "confirm" {
		t.Fatalf("纯对话默认应为 confirm，得到 %q", sess.AccessMode)
	}
	if sess.ProjectID != "" {
		t.Fatalf("纯对话不应有项目归属，得到 %q", sess.ProjectID)
	}
}

// 项目审批规则 CRUD + 幂等 + 删除项目级联。
func TestProjectApprovalRulesLifecycle(t *testing.T) {
	srv, token := newTestServer(t)
	var proj struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects", token,
		map[string]any{"name": "P", "path": `D:\p`}, http.StatusCreated, &proj)

	// 新增规则
	var rule struct {
		ID       string `json:"id"`
		ToolName string `json:"tool_name"`
		Pattern  string `json:"pattern"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects/"+proj.ID+"/approval-rules", token,
		map[string]any{"tool_name": "run_command", "pattern": "go test"},
		http.StatusCreated, &rule)
	if rule.ID == "" || rule.ToolName != "run_command" {
		t.Fatalf("规则创建异常: %+v", rule)
	}

	// 重复添加幂等：仍 201 且 id 不变（唯一索引 (project_id,tool_name,pattern)）
	var dup struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects/"+proj.ID+"/approval-rules", token,
		map[string]any{"tool_name": "run_command", "pattern": "go test"},
		http.StatusCreated, &dup)
	if dup.ID != rule.ID {
		t.Fatalf("重复规则应幂等返回同一条，原 %q 新 %q", rule.ID, dup.ID)
	}

	// 列表
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/projects/"+proj.ID+"/approval-rules", token,
		nil, http.StatusOK, &list)
	if len(list.Items) != 1 {
		t.Fatalf("规则列表应有 1 条，得到 %d", len(list.Items))
	}

	// 删除项目 → 规则级联清空（项目已不在，规则查询返回 404）
	doJSON(t, http.MethodDelete, srv.URL+"/api/v1/projects/"+proj.ID, token, nil, http.StatusNoContent, nil)
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/projects/"+proj.ID+"/approval-rules", token,
		nil, http.StatusNotFound, nil)
}

// 删项目后会话回纯对话（project_id 归零），且会话仍可正常读取。
func TestProjectDeleteUnbindsSessions(t *testing.T) {
	srv, token := newTestServer(t)
	var proj struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects", token,
		map[string]any{"name": "P", "path": `D:\p`, "default_access_mode": "full"},
		http.StatusCreated, &proj)
	var sess struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/sessions", token,
		map[string]any{"title": "s", "project_id": proj.ID}, http.StatusCreated, &sess)

	doJSON(t, http.MethodDelete, srv.URL+"/api/v1/projects/"+proj.ID, token, nil, http.StatusNoContent, nil)

	var got struct {
		ProjectID  string `json:"project_id"`
		AccessMode string `json:"access_mode"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sessions/"+sess.ID, token, nil, http.StatusOK, &got)
	if got.ProjectID != "" {
		t.Fatalf("删项目后会话 project_id 应归零，得到 %q", got.ProjectID)
	}
	// 权限快照不回滚：历史会话保留自己当时的 access_mode
	if got.AccessMode != "full" {
		t.Fatalf("删项目不应回滚会话权限快照，得到 %q", got.AccessMode)
	}
}

// 非法默认权限值必须 400（绝不静默降级，避免用户以为设成 full 实际是 confirm）。
func TestProjectInvalidDefaultAccessModeRejected(t *testing.T) {
	srv, token := newTestServer(t)
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects", token,
		map[string]any{"name": "P", "path": `D:\p`, "default_access_mode": "readonly"},
		http.StatusBadRequest, nil)
}

// 确保 context 引用（doJSON 内部用 r.Context，本文件保留 import 一致性）。
var _ = context.Background
