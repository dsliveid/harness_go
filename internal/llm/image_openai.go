package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"harness-go/pkg/toolapi"
)

// OpenAIImagesConfig OpenAI 兼容图像能力配置。
//
// 与 OpenAICompatConfig 同风格（BaseURL/APIKey/HTTPClient/重试），但独立成一体：
// 图像生成走 `/images/generations`，图像理解走多模态 `/chat/completions`。
type OpenAIImagesConfig struct {
	Name        string       // 适配器名（错误与日志标识），默认 "openai-images"
	BaseURL     string       // 例：https://api.openai.com/v1（尾部斜杠可有可无；缺 /v1 自动补）
	APIKey      string       // 为空则不发送 Authorization
	HTTPClient  *http.Client // nil 用内置（超时由 Timeout / ctx 控制）
	Model       string       // 生成模型，默认 gpt-image-1
	VisionModel string       // 理解模型，默认 gpt-4o-mini
	MaxRetries  int          // 默认 2（仅对可重试错误）
	BaseDelay   time.Duration // 默认 500ms
	Timeout     time.Duration // 单次请求超时，默认 120s（图像生成较慢）
}

// OpenAIImages 实现 toolapi.ImageProvider（OpenAI 兼容图像生成 + 多模态理解）。
//
// 支持边界（诚实声明）：
//   - Generate：**真可用**。POST {base}/images/generations，强制 response_format=b64_json，
//     解 base64 得到二进制；也兼容上游只回 url 的情形（回明确错误，不静默成功）。
//   - Describe：**真可用**，但不复用 ChatStream（ChatRequest.Content 是 string，
//     不支持多模态 content 数组）。这里用一条最小化的独立 HTTP 调用：
//     POST {base}/chat/completions，messages[0].content 为
//     [{type:text},{type:image_url,image_url:{url:data URL}}]，非流式，取 choices[0]。
type OpenAIImages struct {
	name        string
	base        string
	apiKey      string
	client      *http.Client
	model       string
	visionModel string
	maxRetries  int
	baseDelay   time.Duration
	timeout     time.Duration
}

// NewOpenAIImages 创建图像能力实现。
func NewOpenAIImages(cfg OpenAIImagesConfig) (*OpenAIImages, error) {
	base := normalizeImagesBase(cfg.BaseURL)
	if base == "" {
		return nil, fmt.Errorf("llm: 图像能力 BaseURL 不能为空")
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 2
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 500 * time.Millisecond
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	name := cfg.Name
	if name == "" {
		name = "openai-images"
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = "gpt-image-1"
	}
	vision := strings.TrimSpace(cfg.VisionModel)
	if vision == "" {
		vision = "gpt-4o-mini"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	return &OpenAIImages{
		name:        name,
		base:        base,
		apiKey:      cfg.APIKey,
		client:      client,
		model:       model,
		visionModel: vision,
		maxRetries:  cfg.MaxRetries,
		baseDelay:   cfg.BaseDelay,
		timeout:     cfg.Timeout,
	}, nil
}

// Name 返回适配器名。
func (p *OpenAIImages) Name() string { return p.name }

// normalizeImagesBase 规整 BaseURL：去尾部斜杠、剥离误填的接口后缀，缺 /v1 时补上。
func normalizeImagesBase(raw string) string {
	base := strings.TrimSpace(raw)
	if base == "" {
		return ""
	}
	base = strings.TrimRight(base, "/")
	for _, suffix := range []string{"/chat/completions", "/images/generations", "/models"} {
		if strings.HasSuffix(base, suffix) {
			base = strings.TrimSuffix(base, suffix)
			break
		}
	}
	base = strings.TrimRight(base, "/")
	if base == "" {
		return ""
	}
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	return base
}

// ============ Generate ============

type imagesGenRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
}

type imagesGenResponse struct {
	Data []struct {
		B64JSON string `json:"b64_json"`
		URL     string `json:"url"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Generate 调用 {base}/images/generations 生成图像（b64_json 返回）。
func (p *OpenAIImages) Generate(ctx context.Context, req toolapi.ImageGenRequest) (toolapi.ImageGenResult, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return toolapi.ImageGenResult{}, NewError(ErrKindBadRequest, 0, "图像生成: prompt 不能为空")
	}
	model := strings.TrimSpace(req.ModelRef)
	if model == "" {
		model = p.model
	}
	body, err := json.Marshal(imagesGenRequest{
		Model:          model,
		Prompt:         prompt,
		N:              req.N,
		Size:           strings.TrimSpace(req.Size),
		ResponseFormat: "b64_json",
	})
	if err != nil {
		return toolapi.ImageGenResult{}, WrapError(err, ErrKindBadRequest, 0, "图像生成: 构造请求体失败")
	}

	raw, err := p.postJSON(ctx, "/images/generations", body)
	if err != nil {
		return toolapi.ImageGenResult{}, err
	}
	var parsed imagesGenResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return toolapi.ImageGenResult{}, WrapError(err, ErrKindBadRequest, 0, "图像生成: 解析响应失败")
	}
	if parsed.Error != nil && strings.TrimSpace(parsed.Error.Message) != "" {
		return toolapi.ImageGenResult{}, NewError(ErrKindBadRequest, 0, "图像生成被上游拒绝: %s", parsed.Error.Message)
	}
	if len(parsed.Data) == 0 {
		return toolapi.ImageGenResult{}, NewError(ErrKindBadRequest, 0, "图像生成: 上游未返回图像数据")
	}
	item := parsed.Data[0]
	b64 := strings.TrimSpace(item.B64JSON)
	if b64 == "" {
		if strings.TrimSpace(item.URL) != "" {
			return toolapi.ImageGenResult{}, NewError(ErrKindBadRequest, 0,
				"图像生成: 上游仅返回 url（%s），当前实现只支持 b64_json", item.URL)
		}
		return toolapi.ImageGenResult{}, NewError(ErrKindBadRequest, 0, "图像生成: 上游返回的图像数据为空")
	}
	data, dErr := base64.StdEncoding.DecodeString(b64)
	if dErr != nil {
		return toolapi.ImageGenResult{}, WrapError(dErr, ErrKindBadRequest, 0, "图像生成: base64 解码失败")
	}
	if len(data) == 0 {
		return toolapi.ImageGenResult{}, NewError(ErrKindBadRequest, 0, "图像生成: 解码后的图像为空")
	}
	return toolapi.ImageGenResult{Data: data, MIME: sniffMIME(data), Model: model}, nil
}

// ============ Describe（多模态 vision） ============

type visionContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *visionImgURL `json:"image_url,omitempty"`
}

type visionImgURL struct {
	URL string `json:"url"`
}

type visionMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type visionChatRequest struct {
	Model    string          `json:"model"`
	Messages []visionMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

type visionChatResponse struct {
	Choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Describe 用多模态 vision 消息描述/识别图片内容。
//
// 不复用 ChatStream：ChatRequest.Messages[].Content 为 string，无法表达 content 数组。
// 这里发一条最小化的非流式 chat/completions 请求（data URL 内联图片）。
func (p *OpenAIImages) Describe(ctx context.Context, req toolapi.ImageUnderstandRequest) (string, error) {
	if len(req.Image) == 0 {
		return "", NewError(ErrKindBadRequest, 0, "图像识别: 图片数据为空")
	}
	question := strings.TrimSpace(req.Question)
	if question == "" {
		question = "请描述这张图片的内容"
	}
	mime := strings.TrimSpace(req.MIME)
	if mime == "" {
		mime = sniffMIME(req.Image)
	}
	model := strings.TrimSpace(req.ModelRef)
	if model == "" {
		model = p.visionModel
	}
	dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(req.Image)

	body, err := json.Marshal(visionChatRequest{
		Model:  model,
		Stream: false,
		Messages: []visionMessage{{
			Role: "user",
			Content: []visionContentPart{
				{Type: "text", Text: question},
				{Type: "image_url", ImageURL: &visionImgURL{URL: dataURL}},
			},
		}},
	})
	if err != nil {
		return "", WrapError(err, ErrKindBadRequest, 0, "图像识别: 构造请求体失败")
	}
	raw, err := p.postJSON(ctx, "/chat/completions", body)
	if err != nil {
		return "", err
	}
	var parsed visionChatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", WrapError(err, ErrKindBadRequest, 0, "图像识别: 解析响应失败")
	}
	if parsed.Error != nil && strings.TrimSpace(parsed.Error.Message) != "" {
		return "", NewError(ErrKindBadRequest, 0, "图像识别被上游拒绝: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", NewError(ErrKindBadRequest, 0, "图像识别: 上游未返回任何回复")
	}
	text := decodeVisionContent(parsed.Choices[0].Message.Content)
	if strings.TrimSpace(text) == "" {
		return "", NewError(ErrKindBadRequest, 0, "图像识别: 上游返回的文本为空")
	}
	return text, nil
}

// decodeVisionContent 兼容 content 为字符串或分段数组两种形状。
func decodeVisionContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return strings.TrimSpace(string(raw))
	}
	var sb strings.Builder
	for _, p := range parts {
		if strings.TrimSpace(p.Text) == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(p.Text)
	}
	return sb.String()
}

// ============ 传输层 ============

// postJSON 向 {base}{path} 发 POST JSON，带重试（仅可重试错误）与状态分类。
func (p *OpenAIImages) postJSON(ctx context.Context, path string, body []byte) ([]byte, error) {
	url := p.base + path
	var lastErr *Error
	for attempt := 0; attempt <= p.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, WrapError(ctx.Err(), ErrKindContextCancel, 0, "图像请求已取消")
			case <-time.After(p.baseDelay * time.Duration(attempt)):
			}
		}
		raw, err := p.send(ctx, url, body)
		if err == nil {
			return raw, nil
		}
		le := AsError(err)
		if le == nil {
			return nil, err
		}
		lastErr = le
		if !le.Retryable() {
			return nil, le
		}
	}
	if lastErr == nil {
		lastErr = NewError(ErrKindServerError, 0, "图像请求失败（重试耗尽）")
	}
	return nil, lastErr
}

func (p *OpenAIImages) send(ctx context.Context, url string, body []byte) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(cctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, WrapError(err, ErrKindBadRequest, 0, "构造请求失败")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, WrapError(err, ErrKindNetwork, 0, "网络请求失败")
	}
	defer resp.Body.Close()
	raw, rErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if rErr != nil {
		return nil, WrapError(rErr, ErrKindNetwork, resp.StatusCode, "读取响应失败")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, withUpstreamMessage(classifyStatus(resp), raw)
	}
	return raw, nil
}

// withUpstreamMessage 把上游 error.message 拼进错误信息（诊断更直接）。
func withUpstreamMessage(base *Error, raw []byte) *Error {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && strings.TrimSpace(payload.Error.Message) != "" && base != nil {
		base.Message = base.Message + ": " + payload.Error.Message
	}
	return base
}

// sniffMIME 极简魔数判定（PNG/JPEG/GIF/WebP），无法识别时回退 image/png。
func sniffMIME(data []byte) string {
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case len(data) >= 3 && bytes.Equal(data[:3], []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case len(data) >= 6 && (bytes.Equal(data[:6], []byte("GIF87a")) || bytes.Equal(data[:6], []byte("GIF89a"))):
		return "image/gif"
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return "image/png"
}
