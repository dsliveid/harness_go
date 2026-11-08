package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"harness-go/pkg/toolapi"
)

// tinyPNG 最小合法 PNG（1x1），用于魔数判定与 base64 往返。
var tinyPNG = func() []byte {
	raw, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8AAAwAB/wD/1gAAAABJRU5ErkJggg==")
	if err != nil {
		panic(err)
	}
	return raw
}()

func newTestImages(t *testing.T, baseURL string) *OpenAIImages {
	t.Helper()
	p, err := NewOpenAIImages(OpenAIImagesConfig{BaseURL: baseURL, APIKey: "k-test", MaxRetries: 1})
	if err != nil {
		t.Fatalf("构造图像能力失败: %v", err)
	}
	return p
}

// ---------- 1. BaseURL 规整 ----------

func TestNormalizeImagesBase(t *testing.T) {
	cases := map[string]string{
		"https://api.openai.com":                       "https://api.openai.com/v1",
		"https://api.openai.com/":                      "https://api.openai.com/v1",
		"https://api.openai.com/v1":                    "https://api.openai.com/v1",
		"https://api.openai.com/v1/":                   "https://api.openai.com/v1",
		"https://api.openai.com/v1/images/generations": "https://api.openai.com/v1",
		"https://api.openai.com/v1/chat/completions":   "https://api.openai.com/v1",
		"  ":                                           "",
	}
	for in, want := range cases {
		if got := normalizeImagesBase(in); got != want {
			t.Fatalf("normalizeImagesBase(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestNewOpenAIImagesRequiresBaseURL(t *testing.T) {
	if _, err := NewOpenAIImages(OpenAIImagesConfig{BaseURL: "   "}); err == nil {
		t.Fatal("空 BaseURL 必须报错")
	}
}

// ---------- 2. Generate 成功路径（真实 httptest，绝不触外网） ----------

func TestGenerateSuccess(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody imagesGenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` +
			base64.StdEncoding.EncodeToString(tinyPNG) + `"}]}`))
	}))
	defer srv.Close()

	p := newTestImages(t, srv.URL)
	res, err := p.Generate(context.Background(), toolapi.ImageGenRequest{
		Prompt: "一只戴帽子的猫",
		Size:   "1024x1024",
	})
	if err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	if gotPath != "/v1/images/generations" {
		t.Fatalf("请求路径错误: %s", gotPath)
	}
	if gotAuth != "Bearer k-test" {
		t.Fatalf("Authorization 头错误: %s", gotAuth)
	}
	if gotBody.Prompt != "一只戴帽子的猫" || gotBody.Size != "1024x1024" {
		t.Fatalf("请求体不符: %+v", gotBody)
	}
	if gotBody.ResponseFormat != "b64_json" {
		t.Fatalf("必须要求 b64_json，实际 %q", gotBody.ResponseFormat)
	}
	if gotBody.Model != "gpt-image-1" {
		t.Fatalf("默认模型不符: %q", gotBody.Model)
	}
	if string(res.Data) != string(tinyPNG) {
		t.Fatal("解码后的图像字节不符")
	}
	if res.MIME != "image/png" {
		t.Fatalf("MIME 判定错误: %s", res.MIME)
	}
	if res.Model != "gpt-image-1" {
		t.Fatalf("结果模型字段不符: %q", res.Model)
	}
}

func TestGenerateModelRefOverride(t *testing.T) {
	var gotBody imagesGenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(tinyPNG) + `"}]}`))
	}))
	defer srv.Close()

	p := newTestImages(t, srv.URL)
	if _, err := p.Generate(context.Background(), toolapi.ImageGenRequest{
		Prompt:   "x",
		ModelRef: "qwen-image",
	}); err != nil {
		t.Fatalf("Generate 失败: %v", err)
	}
	if gotBody.Model != "qwen-image" {
		t.Fatalf("model_ref 未生效: %q", gotBody.Model)
	}
}

// ---------- 3. Generate 失败路径 ----------

func TestGenerateEmptyPrompt(t *testing.T) {
	p := newTestImages(t, "http://127.0.0.1:1")
	_, err := p.Generate(context.Background(), toolapi.ImageGenRequest{Prompt: "  "})
	if err == nil || !strings.Contains(err.Error(), "prompt 不能为空") {
		t.Fatalf("空 prompt 必须报错: %v", err)
	}
}

func TestGenerateUpstreamOnlyURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"url":"https://example.com/a.png"}]}`))
	}))
	defer srv.Close()

	p := newTestImages(t, srv.URL)
	_, err := p.Generate(context.Background(), toolapi.ImageGenRequest{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "只支持 b64_json") {
		t.Fatalf("仅返回 url 时必须给出明确错误: %v", err)
	}
}

func TestGenerateUpstreamErrorField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"error":{"message":"内容违反安全策略"}}`))
	}))
	defer srv.Close()

	p := newTestImages(t, srv.URL)
	_, err := p.Generate(context.Background(), toolapi.ImageGenRequest{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "内容违反安全策略") {
		t.Fatalf("上游 error 字段必须透出: %v", err)
	}
}

func TestGenerateNoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	p := newTestImages(t, srv.URL)
	_, err := p.Generate(context.Background(), toolapi.ImageGenRequest{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "未返回图像数据") {
		t.Fatalf("空 data 必须报错: %v", err)
	}
}

// 401 不可重试：断言请求只发一次且错误分类为 auth。
func TestGenerateAuthFailureNotRetried(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer srv.Close()

	p := newTestImages(t, srv.URL)
	_, err := p.Generate(context.Background(), toolapi.ImageGenRequest{Prompt: "x"})
	if err == nil {
		t.Fatal("401 必须报错")
	}
	if hits != 1 {
		t.Fatalf("401 不应重试，实际请求 %d 次", hits)
	}
	llmErr := AsError(err)
	if llmErr == nil || llmErr.Kind != ErrKindAuth {
		t.Fatalf("错误分类应为 auth: %+v", err)
	}
	if !strings.Contains(err.Error(), "invalid key") {
		t.Fatalf("上游 message 未透出: %v", err)
	}
}

// 500 可重试：MaxRetries=1 → 共 2 次请求。
func TestGenerateServerErrorRetried(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer srv.Close()

	p := newTestImages(t, srv.URL)
	if _, err := p.Generate(context.Background(), toolapi.ImageGenRequest{Prompt: "x"}); err == nil {
		t.Fatal("500 必须报错")
	}
	if hits != 2 {
		t.Fatalf("500 应重试一次（共 2 次请求），实际 %d", hits)
	}
}

// ---------- 4. Describe（多模态 vision） ----------

func TestDescribeSuccess(t *testing.T) {
	var rawBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("请求路径错误: %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &rawBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"图中是一张 1x1 的像素图"}}]}`))
	}))
	defer srv.Close()

	p := newTestImages(t, srv.URL)
	text, err := p.Describe(context.Background(), toolapi.ImageUnderstandRequest{
		Image:    tinyPNG,
		MIME:     "image/png",
		Question: "图里有什么？",
	})
	if err != nil {
		t.Fatalf("Describe 失败: %v", err)
	}
	if !strings.Contains(text, "1x1 的像素图") {
		t.Fatalf("描述文本不符: %q", text)
	}

	// 校验多模态 content 结构：text 段 + image_url data URL 段
	msgs, _ := rawBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages 长度不符: %d", len(msgs))
	}
	msg, _ := msgs[0].(map[string]any)
	parts, _ := msg["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("content 分段数不符: %d", len(parts))
	}
	p0, _ := parts[0].(map[string]any)
	if p0["type"] != "text" || p0["text"] != "图里有什么？" {
		t.Fatalf("首段应为 text 提问: %+v", p0)
	}
	p1, _ := parts[1].(map[string]any)
	if p1["type"] != "image_url" {
		t.Fatalf("次段应为 image_url: %+v", p1)
	}
	imgURL, _ := p1["image_url"].(map[string]any)
	url, _ := imgURL["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("图片必须是 data URL: %.60s", url)
	}
	if rawBody["stream"] != false {
		t.Fatalf("Describe 应为非流式: %+v", rawBody["stream"])
	}
}

func TestDescribeDefaultQuestion(t *testing.T) {
	var rawBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &rawBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	p := newTestImages(t, srv.URL)
	if _, err := p.Describe(context.Background(), toolapi.ImageUnderstandRequest{Image: tinyPNG}); err != nil {
		t.Fatalf("Describe 失败: %v", err)
	}
	msgs, _ := rawBody["messages"].([]any)
	msg, _ := msgs[0].(map[string]any)
	parts, _ := msg["content"].([]any)
	p0, _ := parts[0].(map[string]any)
	if p0["text"] != "请描述这张图片的内容" {
		t.Fatalf("默认提问不符: %+v", p0["text"])
	}
	// MIME 应由魔数推断为 png
	p1, _ := parts[1].(map[string]any)
	imgURL, _ := p1["image_url"].(map[string]any)
	if url, _ := imgURL["url"].(string); !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("MIME 未按魔数推断: %.60s", url)
	}
}

// content 为分段数组时也能拼接。
func TestDescribeContentParts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":[{"type":"text","text":"第一行"},{"type":"text","text":"第二行"}]}}]}`))
	}))
	defer srv.Close()

	p := newTestImages(t, srv.URL)
	text, err := p.Describe(context.Background(), toolapi.ImageUnderstandRequest{Image: tinyPNG, MIME: "image/png"})
	if err != nil {
		t.Fatalf("Describe 失败: %v", err)
	}
	if text != "第一行\n第二行" {
		t.Fatalf("分段拼接不符: %q", text)
	}
}

func TestDescribeEmptyImage(t *testing.T) {
	p := newTestImages(t, "http://127.0.0.1:1")
	if _, err := p.Describe(context.Background(), toolapi.ImageUnderstandRequest{}); err == nil {
		t.Fatal("空图片必须报错")
	}
}

func TestDescribeEmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	p := newTestImages(t, srv.URL)
	if _, err := p.Describe(context.Background(), toolapi.ImageUnderstandRequest{Image: tinyPNG}); err == nil {
		t.Fatal("无 choices 必须报错")
	}
}

func TestDescribeEmptyText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"   "}}]}`))
	}))
	defer srv.Close()

	p := newTestImages(t, srv.URL)
	if _, err := p.Describe(context.Background(), toolapi.ImageUnderstandRequest{Image: tinyPNG}); err == nil {
		t.Fatal("空文本必须报错（不得静默成功）")
	}
}

// ---------- 5. 接口契约断言 ----------

var _ toolapi.ImageProvider = (*OpenAIImages)(nil)
