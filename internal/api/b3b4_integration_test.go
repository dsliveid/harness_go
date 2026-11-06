package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// B-4：厂商 CRUD + 密钥脱敏 + 通用配置读写 + 统计/日志端点。
func TestProviderAndSettingsEndpoints(t *testing.T) {
	srv, token := newTestServer(t)
	base := srv.URL + "/api/v1"

	// ① 新建厂商：api_key 明文不回传
	var created struct {
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		APIKey    string   `json:"api_key"`
		Models    []string `json:"models"`
		HasAPIKey bool     `json:"has_api_key"`
	}
	doJSON(t, http.MethodPost, base+"/providers", token, map[string]any{
		"name":     "本地 vLLM",
		"base_url": "http://127.0.0.1:8000/v1/",
		"api_key":  "sk-secret-value",
		"models":   []string{"qwen2.5-7b"},
	}, http.StatusCreated, &created)
	if created.ID == "" || created.Name != "本地 vLLM" {
		t.Fatalf("厂商创建异常: %+v", created)
	}
	if created.APIKey != "" {
		t.Fatalf("响应泄露了 api_key 明文: %q", created.APIKey)
	}
	if !created.HasAPIKey {
		t.Fatal("has_api_key 应为 true")
	}

	// ② 列表：密钥仍脱敏
	var list struct {
		Items []struct {
			ID        string `json:"id"`
			BaseURL   string `json:"base_url"`
			APIKey    string `json:"api_key"`
			HasAPIKey bool   `json:"has_api_key"`
		} `json:"items"`
	}
	doJSON(t, http.MethodGet, base+"/providers", token, nil, http.StatusOK, &list)
	if len(list.Items) != 1 {
		t.Fatalf("期望 1 个厂商，得到 %d", len(list.Items))
	}
	if list.Items[0].APIKey != "" || !list.Items[0].HasAPIKey {
		t.Fatalf("列表密钥脱敏异常: %+v", list.Items[0])
	}
	// base_url 尾部斜杠被规范化
	if list.Items[0].BaseURL != "http://127.0.0.1:8000/v1" {
		t.Fatalf("base_url 未规范化: %q", list.Items[0].BaseURL)
	}

	// ③ 更新：不传 api_key 视为保留原密钥
	doJSON(t, http.MethodPatch, base+"/providers/"+created.ID, token, map[string]any{
		"name":    "改名后",
		"enabled": true,
	}, http.StatusOK, &created)
	if created.Name != "改名后" || !created.HasAPIKey {
		t.Fatalf("更新后状态异常: %+v", created)
	}

	// ④ 连通测试：端点存在且失败形态为 ok=false（不返回 5xx）
	var testRes struct {
		OK    bool   `json:"ok"`
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	doJSON(t, http.MethodPost, base+"/providers/"+created.ID+"/test", token, nil, http.StatusOK, &testRes)
	if testRes.OK {
		t.Fatal("不可达端点的连通测试不应返回 ok=true")
	}
	if testRes.Code == "" {
		t.Fatalf("连通测试缺少错误码: %+v", testRes)
	}

	// ⑤ 配置读写：白名单键可写、非法键 400
	var settings struct {
		Items map[string]json.RawMessage `json:"items"`
	}
	doJSON(t, http.MethodPatch, base+"/settings", token, map[string]any{"ui.theme": "dark"},
		http.StatusOK, &settings)
	if string(settings.Items["ui.theme"]) != `"dark"` {
		t.Fatalf("配置写入异常: %s", settings.Items["ui.theme"])
	}
	doJSON(t, http.MethodGet, base+"/settings", token, nil, http.StatusOK, &settings)
	if string(settings.Items["ui.theme"]) != `"dark"` {
		t.Fatalf("配置回读异常: %s", settings.Items["ui.theme"])
	}
	doJSON(t, http.MethodPatch, base+"/settings", token, map[string]any{"unknown.key": 1},
		http.StatusBadRequest, nil)

	// ⑥ 统计与日志
	var stats map[string]any
	doJSON(t, http.MethodGet, base+"/stats", token, nil, http.StatusOK, &stats)
	if _, ok := stats["sessions"]; !ok {
		t.Fatalf("统计缺少 sessions 字段: %v", stats)
	}
	var logs struct {
		Items []struct {
			Level string `json:"level"`
			Text  string `json:"text"`
		} `json:"items"`
	}
	doJSON(t, http.MethodGet, base+"/logs?limit=10", token, nil, http.StatusOK, &logs)
	if logs.Items == nil {
		t.Fatal("日志端点应返回非 null 的 items")
	}

	// ⑦ 删除后列表为空
	doJSON(t, http.MethodDelete, base+"/providers/"+created.ID, token, nil, http.StatusNoContent, nil)
	doJSON(t, http.MethodGet, base+"/providers", token, nil, http.StatusOK, &list)
	if len(list.Items) != 0 {
		t.Fatalf("删除后仍有 %d 个厂商", len(list.Items))
	}
	// 二次删除 → 404（不静默成功）
	doJSON(t, http.MethodDelete, base+"/providers/"+created.ID, token, nil, http.StatusNotFound, nil)
}

// TestProviderSecretAndModels 覆盖「显示/复制密钥」与「获取模型列表」两个新端点：
//   - secret：已配置密钥回明文、未配置回空串；响应必须 no-store
//   - models：上游成功回真实清单；上游失败必须报错（不得回退本地兜底）
func TestProviderSecretAndModels(t *testing.T) {
	// 假上游：/v1/models 返回 OpenAI 形状，/v1/chat/completions 走测试用最小流
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"upstream-a"},{"id":"upstream-b"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	srv, token := newTestServer(t)
	base := srv.URL + "/api/v1"

	var created struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, base+"/providers", token, map[string]any{
		"name":     "上游可用的厂商",
		"base_url": upstream.URL + "/v1",
		"api_key":  "sk-reveal-me",
		"models":   []string{"upstream-a"},
		"enabled":  true,
	}, http.StatusCreated, &created)

	// ① 列表仍脱敏（回归保护：不能因为新增 secret 端点就顺手放开 List）
	var list struct {
		Items []struct {
			APIKey    string `json:"api_key"`
			HasAPIKey bool   `json:"has_api_key"`
		} `json:"items"`
	}
	doJSON(t, http.MethodGet, base+"/providers", token, nil, http.StatusOK, &list)
	if len(list.Items) != 1 || list.Items[0].APIKey != "" || !list.Items[0].HasAPIKey {
		t.Fatalf("列表不应回密钥明文: %+v", list.Items)
	}

	// ② secret 端点回明文 + has_key=true，且响应头 no-store
	var secret struct {
		APIKey string `json:"api_key"`
		HasKey bool   `json:"has_key"`
	}
	doJSON(t, http.MethodGet, base+"/providers/"+created.ID+"/secret", token, nil, http.StatusOK, &secret)
	if secret.APIKey != "sk-reveal-me" || !secret.HasKey {
		t.Fatalf("secret 端点应回明文密钥: %+v", secret)
	}
	if h := providerGetHeader(t, base+"/providers/"+created.ID+"/secret", token); h != "no-store" {
		t.Fatalf("secret 响应 Cache-Control 应为 no-store，实际 %q", h)
	}

	// ③ models 端点：真实转发上游清单（按名称排序后落库口径无关，只断言集合）
	var models struct {
		Items []string `json:"items"`
		Count int      `json:"count"`
	}
	doJSON(t, http.MethodPost, base+"/providers/"+created.ID+"/models", token, map[string]any{}, http.StatusOK, &models)
	if models.Count != 2 || len(models.Items) != 2 {
		t.Fatalf("期望 2 个上游模型，得到 %+v", models)
	}
	got := map[string]bool{models.Items[0]: true, models.Items[1]: true}
	if !got["upstream-a"] || !got["upstream-b"] {
		t.Fatalf("模型清单应来自上游，得到 %v", models.Items)
	}

	// ④ 上游不可达的厂商：models 必须报错（不得回退本地兜底样例）
	var bad struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, base+"/providers", token, map[string]any{
		"name":     "上游不可达",
		"base_url": "http://127.0.0.1:1/v1",
		"api_key":  "sk-x",
		"models":   []string{"m"},
		"enabled":  true,
	}, http.StatusCreated, &bad)
	doJSON(t, http.MethodPost, base+"/providers/"+bad.ID+"/models", token, map[string]any{}, http.StatusServiceUnavailable, nil)

	// ④-2 未保存厂商（草稿 id）+ 草稿 base_url：可直接拉清单（回归：新厂商报「厂商不存在」）
	var draftModels struct {
		Items []string `json:"items"`
		Count int      `json:"count"`
	}
	doJSON(t, http.MethodPost, base+"/providers/pv-unsaved-draft/models", token,
		map[string]any{"base_url": upstream.URL}, http.StatusOK, &draftModels)
	if draftModels.Count != 2 {
		t.Fatalf("未保存厂商凭草稿 base_url 应拉到 2 个模型，得到 %+v", draftModels)
	}

	// ⑤ 未配置密钥的厂商：secret 回空串而非报错（前端据 has_key 置灰复制）
	var noKey struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, base+"/providers", token, map[string]any{
		"name":     "无密钥厂商",
		"base_url": "http://127.0.0.1:1/v1",
		"models":   []string{"m"},
		"enabled":  true,
	}, http.StatusCreated, &noKey)
	doJSON(t, http.MethodGet, base+"/providers/"+noKey.ID+"/secret", token, nil, http.StatusOK, &secret)
	if secret.APIKey != "" || secret.HasKey {
		t.Fatalf("未配置密钥时应回空串: %+v", secret)
	}

	// ⑥ 厂商不存在且无草稿 base_url → 404
	doJSON(t, http.MethodGet, base+"/providers/nonexistent/secret", token, nil, http.StatusNotFound, nil)
	doJSON(t, http.MethodPost, base+"/providers/nonexistent/models", token, map[string]any{}, http.StatusNotFound, nil)
}

// providerGetHeader 取指定端点的 Cache-Control 头（用于断言密钥响应禁止缓存）。
func providerGetHeader(t *testing.T, url, token string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Header.Get("Cache-Control")
}

// B-3：审批决策端点的参数与状态校验（真实「等待审批」链路需工具调用，这里覆盖拒绝路径）。
func TestApprovalDecideValidation(t *testing.T) {
	srv, token := newTestServer(t)
	base := srv.URL + "/api/v1"

	var sess struct {
		ID string `json:"id"`
	}
	doJSON(t, http.MethodPost, base+"/sessions", token,
		map[string]any{"title": "审批测试", "model_ref": "fake-model"}, http.StatusCreated, &sess)

	// ① 非法 decision → 400
	doJSON(t, http.MethodPost,
		fmt.Sprintf("%s/sessions/%s/runs/nonexistent/approvals/a1/decide", base, sess.ID),
		token, map[string]any{"decision": "maybe"}, http.StatusBadRequest, nil)

	// ② Run 不存在 → 404
	doJSON(t, http.MethodPost,
		fmt.Sprintf("%s/sessions/%s/runs/nonexistent/approvals/a1/decide", base, sess.ID),
		token, map[string]any{"decision": "allow_once"}, http.StatusNotFound, nil)

	// ③ 真实 Run（fake provider 秒完成）→ 不在等待审批 → 409
	var run struct {
		RunID string `json:"run_id"`
	}
	doJSON(t, http.MethodPost, fmt.Sprintf("%s/sessions/%s/runs", base, sess.ID), token,
		map[string]any{"input": "hi", "model_ref": "fake-model"}, http.StatusAccepted, &run)
	waitRunStatus(t, base, token, sess.ID, run.RunID, "completed")

	doJSON(t, http.MethodPost,
		fmt.Sprintf("%s/sessions/%s/runs/%s/approvals/whatever/decide", base, sess.ID, run.RunID),
		token, map[string]any{"decision": "allow_once"}, http.StatusConflict, nil)

	// ④ 会话不存在 → 404（不 500）
	doJSON(t, http.MethodPost,
		fmt.Sprintf("%s/sessions/nope/runs/%s/approvals/x/decide", base, run.RunID),
		token, map[string]any{"decision": "deny"}, http.StatusNotFound, nil)
}

// waitRunStatus 轮询 Run 状态直到命中期望值（最多 5 秒）。
func waitRunStatus(t *testing.T, base, token, sid, rid, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/sessions/%s/runs/%s", base, sid, rid), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("查询 Run 失败: %v", err)
		}
		var out struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if out.Status == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("Run %s 未在超时前进入 %s", rid, want)
}

var _ = httptest.NewServer
