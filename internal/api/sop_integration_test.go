package api

import (
	"net/http"
	"strings"
	"testing"
)

// sopCoreResp 核心 SOP 响应（与 openapi SOPCore 对齐）。
type sopCoreResp struct {
	Content        string `json:"content"`
	Source         string `json:"source"`
	Tokens         int    `json:"tokens"`
	Budget         int    `json:"budget"`
	BuiltinContent string `json:"builtin_content"`
	Overridden     bool   `json:"overridden"`
}

// sopCatalogItem 目录条目（与 openapi SOPItem 对齐）。
type sopCatalogItem struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Phase         int    `json:"phase"`
	PhaseLabel    string `json:"phase_label"`
	Category      string `json:"category"`
	CategoryLabel string `json:"category_label"`
	Scope         string `json:"scope"`
	ScopeLabel    string `json:"scope_label"`
	IsHardRule    bool   `json:"is_hard_rule"`
	Description   string `json:"description"`
	DisableEffect string `json:"disable_effect"`
	Variants      []struct {
		Label string `json:"label"`
		Title string `json:"title"`
		Text  string `json:"text"`
	} `json:"variants"`
}

// sopCatalogResp 目录响应（与 openapi SOPCatalog 对齐）。
type sopCatalogResp struct {
	Items    []sopCatalogItem `json:"items"`
	Disabled []string         `json:"disabled"`
	PlanMode string           `json:"plan_mode"`
}

// sopEffectiveResp 生效预览响应（与 openapi SOPEffective 对齐）。
type sopEffectiveResp struct {
	Core struct {
		Source string `json:"source"`
		Tokens int    `json:"tokens"`
	} `json:"core"`
	Rules       []string `json:"rules"`
	ActiveIDs   []string `json:"active_ids"`
	Entries     []any    `json:"entries"`
	TotalTokens int      `json:"total_tokens"`
	SystemPrev  string   `json:"system_preview"`
	EntryLimit  int      `json:"entry_limit"`
}

// TestSOPCoreLifecycle 核心 SOP 四条主链路：
// GET 默认返 builtin → PUT 后转 custom → DELETE 回落 builtin → 超预算 400。
func TestSOPCoreLifecycle(t *testing.T) {
	srv, token := newTestServer(t)
	base := srv.URL + "/api/v1/sop/core"

	// ① 默认：出厂内置，且 builtin_content 必须可用（前端「系统内置」页签渲染它）
	var got sopCoreResp
	doJSON(t, http.MethodGet, base, token, nil, http.StatusOK, &got)
	if got.Source != "builtin" {
		t.Fatalf("默认来源应为 builtin，实际 %s", got.Source)
	}
	if got.Overridden {
		t.Fatal("默认不应处于自定义模式")
	}
	if strings.TrimSpace(got.BuiltinContent) == "" {
		t.Fatal("builtin_content 必须始终可用（切页签不请求也能预览出厂内容）")
	}
	if got.Content != got.BuiltinContent {
		t.Fatal("内置模式下 content 应等于出厂原文")
	}
	if got.Budget != 800 {
		t.Fatalf("预算应为 800，实际 %d", got.Budget)
	}

	// ② 保存自定义 → 来源转 custom，且下一轮立即读到新内容（热生效）
	custom := "你是定制助手。\n规则：先给结论。"
	doJSON(t, http.MethodPut, base, token, map[string]any{"content": custom}, http.StatusOK, &got)
	if got.Source != "custom" || !got.Overridden {
		t.Fatalf("保存后应为自定义模式，实际 source=%s overridden=%v", got.Source, got.Overridden)
	}
	if got.Content != custom {
		t.Fatalf("内容应为新保存值，实际 %q", got.Content)
	}
	// 出厂原文不受影响（重置换回的基准）
	if got.BuiltinContent == custom {
		t.Fatal("builtin_content 不应被自定义内容污染")
	}

	// 再 GET 一次：等价于「下一轮对话」，必须稳定为自定义内容
	var again sopCoreResp
	doJSON(t, http.MethodGet, base, token, nil, http.StatusOK, &again)
	if again.Source != "custom" || again.Content != custom {
		t.Fatalf("重新读取应仍为自定义，实际 source=%s content=%q", again.Source, again.Content)
	}

	// ③ 重置 → 回落出厂内置（完整 undo）
	doJSON(t, http.MethodDelete, base, token, nil, http.StatusOK, &got)
	if got.Source != "builtin" {
		t.Fatalf("重置后来源应为 builtin，实际 %s", got.Source)
	}
	if got.Content != got.BuiltinContent {
		t.Fatal("重置后内容应与出厂逐字一致")
	}
	if got.Overridden {
		t.Fatal("重置后不应仍标记为自定义")
	}

	// ④ 超预算 → 400 且不落盘（拒绝不截断）
	// 预算 800 token：CJK 约 1 token/字，构造 ~2000 字必然超限。
	huge := strings.Repeat("这", 2000)
	doJSON(t, http.MethodPut, base, token, map[string]any{"content": huge}, http.StatusBadRequest, nil)
	var afterFail sopCoreResp
	doJSON(t, http.MethodGet, base, token, nil, http.StatusOK, &afterFail)
	if afterFail.Source != "builtin" {
		t.Fatalf("超预算被拒后应仍为出场内置，实际 %s", afterFail.Source)
	}
}

// TestSOPCatalog 原生 SOP 目录四条主链路：
// GET 目录全集 → PUT 关闭一条（下一轮渲染即少一条）→ 硬规则拒写 400 → 未知 ID 400。
func TestSOPCatalog(t *testing.T) {
	srv, token := newTestServer(t)
	base := srv.URL + "/api/v1/sop/catalog"

	var got sopCatalogResp
	doJSON(t, http.MethodGet, base, token, nil, http.StatusOK, &got)
	if len(got.Items) < 10 {
		t.Fatalf("目录应至少 10 条（对齐老版本 11 条），实际 %d", len(got.Items))
	}
	if len(got.Disabled) != 0 {
		t.Fatalf("初始应为全部启用，实际 disabled=%v", got.Disabled)
	}
	if got.PlanMode != "standard" {
		t.Fatalf("初始规划模式应为 standard，实际 %s", got.PlanMode)
	}
	// 每条必须带齐前端渲染所需字段（缺一个界面就会空白）。
	hard := ""
	soft := ""
	for _, it := range got.Items {
		if it.ID == "" || it.Name == "" || it.Description == "" || it.DisableEffect == "" {
			t.Fatalf("目录条目字段缺失: %+v", it)
		}
		if len(it.Variants) == 0 {
			t.Fatalf("%s 缺少 variants", it.ID)
		}
		if it.Name != it.Variants[0].Title {
			t.Fatalf("%s 的 name 未由 variants[0].title 派生", it.ID)
		}
		if it.IsHardRule && hard == "" {
			hard = it.ID
		}
		if !it.IsHardRule && soft == "" && it.Scope == "universal" {
			soft = it.ID
		}
	}
	if hard == "" || soft == "" {
		t.Fatalf("目录应同时含硬规则与可关闭规则（hard=%q soft=%q）", hard, soft)
	}

	// ① 关闭一条非硬规则 → 生效预览立即少一条（热生效，无需重启）
	var before sopEffectiveResp
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sop/effective", token, nil, http.StatusOK, &before)

	var saved sopCatalogResp
	doJSON(t, http.MethodPut, base, token, map[string]any{"disabled": []string{soft}}, http.StatusOK, &saved)
	if len(saved.Disabled) != 1 || saved.Disabled[0] != soft {
		t.Fatalf("关闭后 disabled 应为 [%s]，实际 %v", soft, saved.Disabled)
	}

	var after sopEffectiveResp
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sop/effective", token, nil, http.StatusOK, &after)
	if len(after.Rules) != len(before.Rules)-1 {
		t.Fatalf("关闭一条规则后生效规则应少 1 条（%d → %d）", len(before.Rules), len(after.Rules))
	}
	for _, id := range after.ActiveIDs {
		if id == soft {
			t.Fatalf("已关闭规则 %s 仍出现在生效清单中", soft)
		}
	}
	for _, sec := range after.Rules {
		if strings.Contains(sec, "【"+nameOf(got.Items, soft)+"】") {
			t.Fatalf("已关闭规则 %s 仍出现在渲染文本中", soft)
		}
	}

	// ② 硬规则不可关闭 → 400，且状态不被污染
	doJSON(t, http.MethodPut, base, token, map[string]any{"disabled": []string{hard}},
		http.StatusBadRequest, nil)
	var still sopCatalogResp
	doJSON(t, http.MethodGet, base, token, nil, http.StatusOK, &still)
	if len(still.Disabled) != 1 || still.Disabled[0] != soft {
		t.Fatalf("硬规则被拒后状态应保持不变，实际 %v", still.Disabled)
	}

	// ③ 未知 ID → 400（拼错 ID 会静默失效，必须在入口拒绝）
	doJSON(t, http.MethodPut, base, token, map[string]any{"disabled": []string{"no_such_rule"}},
		http.StatusBadRequest, nil)
	// ④ 重复 ID → 400
	doJSON(t, http.MethodPut, base, token, map[string]any{"disabled": []string{soft, soft}},
		http.StatusBadRequest, nil)

	// ⑤ 切到 always_plan：方案先行规则换措辞，且模式落库可见
	var planned sopCatalogResp
	doJSON(t, http.MethodPut, base, token, map[string]any{"disabled": []string{}, "plan_mode": "always_plan"},
		http.StatusOK, &planned)
	if planned.PlanMode != "always_plan" {
		t.Fatalf("规划模式应已保存为 always_plan，实际 %s", planned.PlanMode)
	}
	var planEff sopEffectiveResp
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sop/effective", token, nil, http.StatusOK, &planEff)
	joined := strings.Join(planEff.Rules, "\n")
	if !strings.Contains(joined, "Always Plan 模式") {
		t.Fatal("always_plan 模式下方案先行规则应渲染确认门禁变体")
	}
	if strings.Contains(joined, "Always Proceed 模式") {
		t.Fatal("always_plan 模式下不应渲染 always_proceed 变体")
	}

	// ⑥ 全部启用 + 回落 standard（完整 undo）
	doJSON(t, http.MethodPut, base, token, map[string]any{"disabled": []string{}, "plan_mode": "standard"},
		http.StatusOK, &planned)
	if len(planned.Disabled) != 0 || planned.PlanMode != "standard" {
		t.Fatalf("重置后应全启用 + standard，实际 disabled=%v mode=%s", planned.Disabled, planned.PlanMode)
	}
}

// nameOf 取目录条目的界面标题（用于渲染文本断言）。
func nameOf(items []sopCatalogItem, id string) string {
	for _, it := range items {
		if it.ID == id {
			return it.Name
		}
	}
	return ""
}

// TestSOPEffective 生效预览：不跑对话即可确认注入内容与 token 合计。
func TestSOPEffective(t *testing.T) {
	srv, token := newTestServer(t)

	var view sopEffectiveResp
	doJSON(t, http.MethodGet, srv.URL+"/api/v1/sop/effective", token, nil, http.StatusOK, &view)

	if view.Core.Source != "builtin" {
		t.Fatalf("未自定义时核心来源应为 builtin，实际 %s", view.Core.Source)
	}
	if view.Core.Tokens <= 0 {
		t.Fatal("核心 SOP token 应 > 0")
	}
	if view.TotalTokens < view.Core.Tokens {
		t.Fatal("总 token 应不小于核心部分")
	}
	if strings.TrimSpace(view.SystemPrev) == "" {
		t.Fatal("system_preview 必须给出真实注入文本预览")
	}
	if view.EntryLimit != 6 {
		t.Fatalf("条目上限应为 6，实际 %d", view.EntryLimit)
	}
	if len(view.Entries) != 0 {
		t.Fatal("未绑定工作区时不应有项目级条目")
	}
	// 原生 SOP 规则必须真的进了预览（否则「补全 SOP」这件事对模型不可见）。
	if len(view.Rules) == 0 {
		t.Fatal("生效预览应包含原生 SOP 规则行")
	}
	if len(view.ActiveIDs) != len(view.Rules) {
		t.Fatalf("生效 ID 数应与规则条数一致（%d vs %d）", len(view.ActiveIDs), len(view.Rules))
	}
	joined := strings.Join(view.Rules, "\n")
	for _, anchor := range []string{"Todo 先行", "进程生命周期", "边读边记"} {
		if !strings.Contains(joined, anchor) {
			t.Fatalf("生效规则缺少关键锚点: %s", anchor)
		}
	}
	if !strings.Contains(view.SystemPrev, "## 工作规则") {
		t.Fatal("系统提示预览应包含「## 工作规则」段")
	}
}
