package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"harness-go/internal/model"
	"harness-go/internal/store"
)

// sseTestUpstream 假 SSE 上游：记录收到的 model 与会话路由头，回复一个分片即完成。
func sseTestUpstream(t *testing.T, captured *struct {
	Model     string
	SessionID string
}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		captured.Model = body.Model
		captured.SessionID = r.Header.Get("x-opencode-session")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestProviderTestDraftOverrides 连通测试的草稿覆盖语义（所见即所测）：
//   - 未落库厂商 + base_url：按请求配置直接测（回归：新厂商未保存也能测）；
//   - 已落库厂商 + models 覆盖：草稿新加的未保存模型优先生效（回归：添加模型未保存报「未配置模型」）；
//   - 未落库且无 base_url：404；
//   - 无任何可用模型：ok=false + missing_model（业务失败，非错误）。
func TestProviderTestDraftOverrides(t *testing.T) {
	ctx := context.Background()
	main, err := store.OpenMain(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	// Windows：TempDir 清理要求先关闭数据库句柄（后注册先执行，先于目录删除）
	t.Cleanup(func() { _ = main.Close() })
	svc := NewProviderService(ProviderServiceOptions{Main: main})

	// ① 未落库厂商：仅凭草稿 base_url + models 直接测
	captured := &struct {
		Model     string
		SessionID string
	}{}
	srv := sseTestUpstream(t, captured)
	res, err := svc.Test(ctx, "pv-unsaved-draft", ProviderTestInput{
		BaseURL: srv.URL,
		Models:  []string{"draft-model"},
	})
	if err != nil {
		t.Fatalf("未落库厂商带 base_url 应直接测试: %v", err)
	}
	if !res.OK || res.Model != "draft-model" {
		t.Fatalf("期望 ok=true model=draft-model，得到 %+v", res)
	}
	if captured.Model != "draft-model" {
		t.Fatalf("上游应收到草稿模型 %q，得到 %q", "draft-model", captured.Model)
	}
	if captured.SessionID == "" {
		t.Fatal("上游应收到会话路由头 x-opencode-session")
	}

	// ② 已落库厂商：models 覆盖优先于库内值（草稿新加模型未保存）
	saved, err := svc.Create(ctx, model.Provider{Name: "p1", BaseURL: srv.URL, Models: []string{"saved-old"}})
	if err != nil {
		t.Fatalf("创建厂商失败: %v", err)
	}
	res2, err := svc.Test(ctx, saved.ID, ProviderTestInput{Models: []string{"draft-new"}})
	if err != nil {
		t.Fatalf("已落库厂商带 models 覆盖应成功: %v", err)
	}
	if !res2.OK || res2.Model != "draft-new" {
		t.Fatalf("期望草稿模型 draft-new 生效，得到 %+v", res2)
	}
	if captured.Model != "draft-new" {
		t.Fatalf("上游应收到草稿模型 draft-new（而非库内 saved-old），得到 %q", captured.Model)
	}

	// ③ 未落库且无 base_url：404
	if _, err := svc.Test(ctx, "pv-nowhere", ProviderTestInput{}); err == nil {
		t.Fatal("未落库且无 base_url 应返回 404 类错误")
	}

	// ④ 落库厂商但无模型、无覆盖：missing_model 业务失败
	empty, err := svc.Create(ctx, model.Provider{Name: "p2", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("创建空模型厂商失败: %v", err)
	}
	res4, err := svc.Test(ctx, empty.ID, ProviderTestInput{})
	if err != nil {
		t.Fatalf("missing_model 应以 ok=false 返回而非错误: %v", err)
	}
	if res4.OK || res4.Code != "missing_model" {
		t.Fatalf("期望 ok=false code=missing_model，得到 %+v", res4)
	}
}
