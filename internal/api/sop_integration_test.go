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

// TestSOPEffective 生效预览：不跑对话即可确认注入内容与 token 合计。
func TestSOPEffective(t *testing.T) {
	srv, token := newTestServer(t)

	var view struct {
		Core struct {
			Source string `json:"source"`
			Tokens int    `json:"tokens"`
		} `json:"core"`
		Entries     []any  `json:"entries"`
		TotalTokens int    `json:"total_tokens"`
		SystemPrev  string `json:"system_preview"`
		EntryLimit  int    `json:"entry_limit"`
	}
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
}
