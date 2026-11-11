package api

import (
	"net/http"
	"testing"

	"harness-go/internal/store"
)

// ============ DELETE /workspaces/{wid}?migrate_refs= 契约 ============
//
// 用户报告：Web 端移除工作空间后刷新又出现。修复的第二层是「点确认就真删」：
// handler 必须把 migrate_refs 透传给 service，否则服务端仍走严格模式 → 409 → 删不掉。
//
// 本文件从 HTTP 层锁死这条契约，防止哪天有人「顺手简化」掉这个 query 参数。

// TestWorkspaceDeleteWithMigrateRefs 建空间 → 建会话引用 → 带 migrate_refs=true 删除 → 204 且列表不再含它。
func TestWorkspaceDeleteWithMigrateRefs(t *testing.T) {
	srv, token := newTestServer(t)
	base := srv.URL + "/api/v1"

	// ① 建空间
	var ws struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, base+"/workspaces", token, map[string]any{
		"name": "e2e-移除测试",
		"kind": "desktop",
	}, http.StatusCreated, &ws)
	if ws.ID == "" {
		t.Fatal("创建工作空间应返回 id")
	}

	// ② 建一个归属该空间的会话（模拟「空间下有对话」的真实场景）
	doJSON(t, http.MethodPost, base+"/sessions", token, map[string]any{
		"title":        "e2e 会话",
		"workspace_id": ws.ID,
	}, http.StatusCreated, nil)

	// ③ 严格模式：有引用必须 409（旧契约不可回退）
	doJSON(t, http.MethodDelete, base+"/workspaces/"+ws.ID, token, nil, http.StatusConflict, nil)

	// ④ 确认移除：migrate_refs=true → 204
	doJSON(t, http.MethodDelete, base+"/workspaces/"+ws.ID+"?migrate_refs=true", token, nil, http.StatusNoContent, nil)

	// ⑤ 列表不再包含它（= 刷新界面不会再出现）
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	doJSON(t, http.MethodGet, base+"/workspaces", token, nil, http.StatusOK, &list)
	for _, it := range list.Items {
		if it.ID == ws.ID {
			t.Fatalf("migrate_refs=true 删除后列表仍返回 %q —— 刷新会复活", ws.ID)
		}
	}

	// ⑥ 再删一次 → 404（不静默成功）
	doJSON(t, http.MethodDelete, base+"/workspaces/"+ws.ID+"?migrate_refs=true", token, nil, http.StatusNotFound, nil)
}

// TestWorkspaceDeleteNoPrivilegeEscalation 级联删除后会话必须**被删除**，且绝不能被迁到 ws-server。
//
// ws-server 的能力天花板是 full（可写 + 命令），把 mobile/离线空间的会话迁进去
// 等于静默提权（违反 D8）。这条用例锁死「级联删除」而非「迁移」的语义。
func TestWorkspaceDeleteNoPrivilegeEscalation(t *testing.T) {
	srv, token := newTestServer(t)
	base := srv.URL + "/api/v1"

	var ws struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, base+"/workspaces", token, map[string]any{
		"name": "级联回归", "kind": "mobile",
	}, http.StatusCreated, &ws)

	var sess struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, base+"/sessions", token, map[string]any{
		"title": "mobile 会话", "workspace_id": ws.ID,
	}, http.StatusCreated, &sess)

	doJSON(t, http.MethodDelete, base+"/workspaces/"+ws.ID+"?migrate_refs=true", token, nil, http.StatusNoContent, nil)

	// 会话必须被级联删除：详情 404，且不再出现在会话列表里。
	doJSON(t, http.MethodGet, base+"/sessions/"+sess.ID, token, nil, http.StatusNotFound, nil)

	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	doJSON(t, http.MethodGet, base+"/sessions", token, nil, http.StatusOK, &list)
	for _, it := range list.Items {
		if it.ID == sess.ID {
			t.Fatalf("会话 %q 应已被级联删除，却仍在列表里", sess.ID)
		}
	}
}

// TestWorkspaceDeleteBuiltinStillConflict 内置空间即使带 migrate_refs=true 也必须 409。
func TestWorkspaceDeleteBuiltinStillConflict(t *testing.T) {
	srv, token := newTestServer(t)
	doJSON(t, http.MethodDelete,
		srv.URL+"/api/v1/workspaces/"+store.BuiltinWorkspaceServerID+"?migrate_refs=true",
		token, nil, http.StatusConflict, nil)
}
