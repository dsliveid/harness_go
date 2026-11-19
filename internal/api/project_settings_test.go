package api

// 项目设置全量持久化（0014）端到端验收。
//
// ============ 为什么需要这组测试 ============
//
// 用户诉求：「项目设置需要长期保存」。改造前 6 块配置的持久化状态极不一致：
//
//	项目约束(constraints)  → 仅前端 localStorage
//	规划模式(planMode)     → 仅前端 localStorage
//	关联项目(projectLinks) → 仅前端 localStorage
//	数据权限(dataGrants)   → **仅前端内存，刷新即丢**
//
// 前三者「换个浏览器就没了」，第四个「刷新就没了」。本文件把「落库 + 读回」
// 这条链路钉死，防止哪天有人把 service/store 的写法改回纯内存而无人察觉。
//
// 判据口径：**写完必须能从 GET 读回同样的值**（只断言 204 是不够的，
// 「返回成功但没写库」正是本类缺陷最典型的形态）。

import (
	"net/http"
	"testing"
)

// 项目约束与规划模式：PATCH 后必须能读回，且支持显式清空约束。
func TestProjectConstraintsAndPlanModePersist(t *testing.T) {
	srv, token := newTestServer(t)

	var proj struct {
		ID          string `json:"id"`
		Constraints string `json:"constraints"`
		PlanMode    string `json:"plan_mode"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects", token,
		map[string]any{"name": "持久化项目", "path": `D:\persist`}, http.StatusCreated, &proj)
	if proj.ID == "" {
		t.Fatalf("项目创建失败: %+v", proj)
	}
	// 新建项目必须是空约束 + standard 规划模式（不能带出别人的值）
	if proj.Constraints != "" || proj.PlanMode != "standard" {
		t.Fatalf("新项目默认值异常: constraints=%q plan_mode=%q", proj.Constraints, proj.PlanMode)
	}

	// 写入约束与规划模式
	doJSON(t, http.MethodPatch, srv.URL+"/api/v1/projects/"+proj.ID, token,
		map[string]any{"constraints": "必须使用 Go 1.22+；禁止引入新依赖", "plan_mode": "always_plan"},
		http.StatusNoContent, nil)

	var got struct {
		Constraints string `json:"constraints"`
		PlanMode    string `json:"plan_mode"`
		Name        string `json:"name"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/projects/"+proj.ID, token, nil, http.StatusOK, &got)
	if got.Constraints != "必须使用 Go 1.22+；禁止引入新依赖" {
		t.Fatalf("项目约束未持久化，读回 %q", got.Constraints)
	}
	if got.PlanMode != "always_plan" {
		t.Fatalf("规划模式未持久化，读回 %q", got.PlanMode)
	}
	// 只改约束不得把名字清空（PATCH 语义）
	if got.Name != "持久化项目" {
		t.Fatalf("仅改约束不应影响名称，读回 %q", got.Name)
	}

	// 清单里也要带上（列表与详情同源）
	var list struct {
		Items []struct {
			ID          string `json:"id"`
			Constraints string `json:"constraints"`
			PlanMode    string `json:"plan_mode"`
		} `json:"items"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/projects", token, nil, http.StatusOK, &list)
	found := false
	for _, p := range list.Items {
		if p.ID == proj.ID {
			found = true
			if p.PlanMode != "always_plan" || p.Constraints == "" {
				t.Fatalf("列表未带出项目设置: %+v", p)
			}
		}
	}
	if !found {
		t.Fatal("列表里找不到刚创建的项目")
	}

	// 显式清空约束必须生效（指针语义的意义所在）
	doJSON(t, http.MethodPatch, srv.URL+"/api/v1/projects/"+proj.ID, token,
		map[string]any{"constraints": ""}, http.StatusNoContent, nil)
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/projects/"+proj.ID, token, nil, http.StatusOK, &got)
	if got.Constraints != "" {
		t.Fatalf("空串应清空约束，读回 %q", got.Constraints)
	}
	// 清空约束不得顺手把规划模式重置
	if got.PlanMode != "always_plan" {
		t.Fatalf("清空约束不应影响规划模式，读回 %q", got.PlanMode)
	}
}

// 非法 plan_mode 必须 400（绝不静默降级成 standard）。
func TestProjectInvalidPlanModeRejected(t *testing.T) {
	srv, token := newTestServer(t)
	var proj struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects", token,
		map[string]any{"name": "P", "path": `D:\p`}, http.StatusCreated, &proj)

	doJSON(t, http.MethodPatch, srv.URL+"/api/v1/projects/"+proj.ID, token,
		map[string]any{"plan_mode": "yolo"}, http.StatusBadRequest, nil)

	// 非法值被拒后，原值必须仍是 standard（没被写坏）
	var got struct {
		PlanMode string `json:"plan_mode"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/projects/"+proj.ID, token, nil, http.StatusOK, &got)
	if got.PlanMode != "standard" {
		t.Fatalf("非法值应被拒绝且不改动原值，读回 %q", got.PlanMode)
	}
}

// 关联项目：增 / 查 / 改 / 删 全链路落库。
func TestProjectLinksPersist(t *testing.T) {
	srv, token := newTestServer(t)
	var proj struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects", token,
		map[string]any{"name": "主项目", "path": `D:\main`}, http.StatusCreated, &proj)

	// 空列表起步
	var listed struct {
		Items []struct {
			ID          string `json:"id"`
			Path        string `json:"path"`
			Description string `json:"description"`
		} `json:"items"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/projects/"+proj.ID+"/links", token, nil, http.StatusOK, &listed)
	if len(listed.Items) != 0 {
		t.Fatalf("新项目不应有关联项目，得到 %d 条", len(listed.Items))
	}

	// 新增
	var link struct {
		ID          string `json:"id"`
		Path        string `json:"path"`
		Description string `json:"description"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects/"+proj.ID+"/links", token,
		map[string]any{"path": `F:\WorkSpace\Other\harness_mini`, "description": "旧版本"},
		http.StatusCreated, &link)
	if link.ID == "" || link.Path == "" {
		t.Fatalf("关联项目创建结果异常: %+v", link)
	}

	// 路径必填 → 400
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects/"+proj.ID+"/links", token,
		map[string]any{"path": "  "}, http.StatusBadRequest, nil)

	// 读回
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/projects/"+proj.ID+"/links", token, nil, http.StatusOK, &listed)
	if len(listed.Items) != 1 || listed.Items[0].Path != link.Path {
		t.Fatalf("关联项目未持久化: %+v", listed.Items)
	}

	// 改
	doJSON(t, http.MethodPatch, srv.URL+"/api/v1/projects/"+proj.ID+"/links/"+link.ID, token,
		map[string]any{"path": `F:\WorkSpace\Other\harness_desktop`, "description": "桌面端"},
		http.StatusNoContent, nil)
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/projects/"+proj.ID+"/links", token, nil, http.StatusOK, &listed)
	if listed.Items[0].Path != `F:\WorkSpace\Other\harness_desktop` || listed.Items[0].Description != "桌面端" {
		t.Fatalf("关联项目更新未持久化: %+v", listed.Items[0])
	}

	// 删
	doJSON(t, http.MethodDelete, srv.URL+"/api/v1/projects/"+proj.ID+"/links/"+link.ID, token,
		nil, http.StatusNoContent, nil)
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/projects/"+proj.ID+"/links", token, nil, http.StatusOK, &listed)
	if len(listed.Items) != 0 {
		t.Fatalf("关联项目删除后应清空，得到 %d 条", len(listed.Items))
	}
}

// 跨项目保护：不能借 A 项目的 pid 改/删 B 项目的关联项目。
func TestProjectLinkCrossProjectRejected(t *testing.T) {
	srv, token := newTestServer(t)
	var pa, pb struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects", token,
		map[string]any{"name": "A", "path": `D:\a`}, http.StatusCreated, &pa)
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects", token,
		map[string]any{"name": "B", "path": `D:\b`}, http.StatusCreated, &pb)

	var link struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects/"+pa.ID+"/links", token,
		map[string]any{"path": `D:\shared`}, http.StatusCreated, &link)

	// 用 B 的 pid 去改 A 的关联项目 → 404（不泄露、不误改）
	doJSON(t, http.MethodPatch, srv.URL+"/api/v1/projects/"+pb.ID+"/links/"+link.ID, token,
		map[string]any{"path": `D:\hacked`}, http.StatusNotFound, nil)
	doJSON(t, http.MethodDelete, srv.URL+"/api/v1/projects/"+pb.ID+"/links/"+link.ID, token,
		nil, http.StatusNotFound, nil)
	// A 的关联项目必须完好
	var listed struct {
		Items []struct {
			Path string `json:"path"`
		} `json:"items"`
	}
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/projects/"+pa.ID+"/links", token, nil, http.StatusOK, &listed)
	if len(listed.Items) != 1 || listed.Items[0].Path != `D:\shared` {
		t.Fatalf("跨项目请求不应影响原项目关联: %+v", listed.Items)
	}
}

// 数据权限：PUT 幂等 upsert + 读回；未设置的 scope 不出现在列表里。
func TestProjectDataGrantsPersist(t *testing.T) {
	srv, token := newTestServer(t)
	var proj struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects", token,
		map[string]any{"name": "权限项目", "path": `D:\grant`}, http.StatusCreated, &proj)

	var listed struct {
		Items []struct {
			Scope string `json:"scope"`
			Mode  string `json:"mode"`
		} `json:"items"`
	}
	// 🚨 不预置任何行：未表态必须是「无记录」，由前端按 ask 兜底
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/projects/"+proj.ID+"/data-grants", token, nil, http.StatusOK, &listed)
	if len(listed.Items) != 0 {
		t.Fatalf("不应预置任何数据权限行，得到 %+v", listed.Items)
	}

	// 设置 allow
	doJSON(t, http.MethodPut, srv.URL+"/api/v1/projects/"+proj.ID+"/data-grants/fs_outside", token,
		map[string]any{"mode": "allow"}, http.StatusNoContent, nil)
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/projects/"+proj.ID+"/data-grants", token, nil, http.StatusOK, &listed)
	if len(listed.Items) != 1 || listed.Items[0].Scope != "fs_outside" || listed.Items[0].Mode != "allow" {
		t.Fatalf("数据权限未持久化: %+v", listed.Items)
	}

	// 同 scope 再设一次 → 幂等更新，不得产生第二行
	doJSON(t, http.MethodPut, srv.URL+"/api/v1/projects/"+proj.ID+"/data-grants/fs_outside", token,
		map[string]any{"mode": "deny"}, http.StatusNoContent, nil)
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/projects/"+proj.ID+"/data-grants", token, nil, http.StatusOK, &listed)
	if len(listed.Items) != 1 || listed.Items[0].Mode != "deny" {
		t.Fatalf("重复设置应幂等更新同一行，得到 %+v", listed.Items)
	}

	// 非法 scope / 非法 mode → 400
	doJSON(t, http.MethodPut, srv.URL+"/api/v1/projects/"+proj.ID+"/data-grants/unknown_scope", token,
		map[string]any{"mode": "allow"}, http.StatusBadRequest, nil)
	doJSON(t, http.MethodPut, srv.URL+"/api/v1/projects/"+proj.ID+"/data-grants/db_query", token,
		map[string]any{"mode": "maybe"}, http.StatusBadRequest, nil)
}

// 删除项目必须级联清掉关联项目与数据权限（否则重建会继承幽灵配置）。
func TestProjectDeleteCascadesSettings(t *testing.T) {
	srv, token := newTestServer(t)
	var proj struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects", token,
		map[string]any{"name": "待删项目", "path": `D:\gone`}, http.StatusCreated, &proj)

	doJSON(t, http.MethodPost, srv.URL+"/api/v1/projects/"+proj.ID+"/links", token,
		map[string]any{"path": `D:\linked`}, http.StatusCreated, nil)
	doJSON(t, http.MethodPut, srv.URL+"/api/v1/projects/"+proj.ID+"/data-grants/net_egress", token,
		map[string]any{"mode": "allow"}, http.StatusNoContent, nil)

	doJSON(t, http.MethodDelete, srv.URL+"/api/v1/projects/"+proj.ID, token, nil, http.StatusNoContent, nil)

	// 项目已删 → 子资源端点一律 404（项目不存在被前置校验拦住）
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/projects/"+proj.ID+"/links", token, nil, http.StatusNotFound, nil)
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/projects/"+proj.ID+"/data-grants", token, nil, http.StatusNotFound, nil)
}
