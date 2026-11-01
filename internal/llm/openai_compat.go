package llm

import (
	"bufio"
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// OpenAICompatConfig OpenAI 兼容端点配置（OpenAI / DeepSeek / Qwen / 本地 vLLM 等）。
type OpenAICompatConfig struct {
	Name              string            // 适配器名（日志用）
	BaseURL           string            // 例：https://api.openai.com/v1（尾部斜杠可有可无）
	APIKey            string            // 为空则不发送 Authorization
	HTTPClient        *http.Client      // nil 用内置（不设全局超时，超时由 ctx 与首 token 定时器控制）
	MaxRetries        int               // 默认 2（仅对可重试错误）
	BaseDelay         time.Duration     // 默认 500ms
	FirstTokenTimeout time.Duration     // 默认 60s；0 表示不限制
	ExtraHeaders      map[string]string // 额外请求头（如厂商要求的版本头），可覆盖内置会话头
	// SessionID 会话路由 ID（随 chat 请求以 x-opencode-session / x-session-id 头发送）。
	// opencode zen 等网关强制要求该头做路由（缺失直接 400 MissingSessionID）；
	// 对不认识的厂商无副作用。为空自动生成 sess-<32hex>（对齐桌面端 llm.rs）。
	SessionID string
}

// OpenAICompat OpenAI Chat Completions 协议适配器（流式）。
type OpenAICompat struct {
	name              string
	baseURL           string
	endpoint          string
	apiKey            string
	sessionID         string
	client            *http.Client
	maxRetries        int
	baseDelay         time.Duration
	firstTokenTimeout time.Duration
	extraHeaders      map[string]string
}

// NewOpenAICompat 创建适配器。
func NewOpenAICompat(cfg OpenAICompatConfig) (*OpenAICompat, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("llm: BaseURL 不能为空")
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
	if cfg.FirstTokenTimeout == 0 {
		cfg.FirstTokenTimeout = 60 * time.Second
	}
	name := cfg.Name
	if name == "" {
		name = "openai-compat"
	}
	sessionID := strings.TrimSpace(cfg.SessionID)
	if sessionID == "" {
		sessionID = NewSessionID()
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	return &OpenAICompat{
		name:              name,
		baseURL:           strings.TrimRight(cfg.BaseURL, "/"),
		endpoint:          strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions",
		apiKey:            cfg.APIKey,
		sessionID:         sessionID,
		client:            client,
		maxRetries:        cfg.MaxRetries,
		baseDelay:         cfg.BaseDelay,
		firstTokenTimeout: cfg.FirstTokenTimeout,
		extraHeaders:      cfg.ExtraHeaders,
	}, nil
}

// NewSessionID 生成会话路由 ID：sess-<32hex>（对齐桌面端 llm.rs 的 sess-{uuid simple}，
// 不引入 uuid 依赖）。
func NewSessionID() string {
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		// crypto/rand 失败极罕见（系统熵源不可用）：退化为时间戳 + 固定后缀，保住「非空且可区分」
		return fmt.Sprintf("sess-%x-fallback", time.Now().UnixNano())
	}
	return "sess-" + hex.EncodeToString(b)
}

// Name 返回适配器名。
func (p *OpenAICompat) Name() string { return p.name }

// ListModels 拉取上游模型清单（用于「设置 → 模型 → 获取模型列表」）。
//
// Base URL 规整与候选顺序对齐桌面端 src-tauri/src/llm.rs::list_models：
// 先剥离误填的 `/chat/completions`、`/messages`、`/responses` 后缀，
// 再依次尝试 `{base}/models`、`{base}/v1/models`（base 未含 /v1 时）、`{base}/api/tags`（Ollama）。
//
// 兼容两种响应形状：OpenAI `{"data":[{"id":"..."}]}`、Ollama `{"models":[{"name":"..."}]}`。
// 全部候选失败时返回聚合错误（含每个候选的最后状态），**绝不回退本地目录/样例数据**。
func (p *OpenAICompat) ListModels(ctx context.Context) ([]string, error) {
	base := normalizeModelsBase(p.baseURL)
	if base == "" {
		return nil, NewError(ErrKindBadRequest, 0, "Base URL 不能为空")
	}
	var candidates []string
	if strings.HasSuffix(base, "/models") {
		candidates = []string{base}
	} else {
		candidates = []string{base + "/models"}
		if !strings.Contains(base, "/v1") && !strings.Contains(base, "/v4") {
			candidates = append(candidates, base+"/v1/models")
		}
		candidates = append(candidates, base+"/api/tags")
	}

	var failures []string
	var lastErr error
	for _, url := range candidates {
		list, err := p.fetchModelList(ctx, url)
		if err != nil {
			failures = append(failures, summarizeModelFetch(err))
			lastErr = err
			continue
		}
		return list, nil
	}
	msg := "获取模型列表失败"
	if len(failures) > 0 {
		msg += "：" + strings.Join(failures, "；")
	}
	if lastErr != nil {
		return nil, WrapError(lastErr, AsError(lastErr).Kind, AsError(lastErr).StatusCode, "%s", msg)
	}
	return nil, NewError(ErrKindNetwork, 0, "%s", msg)
}

// normalizeModelsBase 归一化「获取模型列表」用的 Base URL（去尾斜杠 + 剥离具体端点后缀）。
func normalizeModelsBase(raw string) string {
	base := strings.TrimSpace(raw)
	if base == "" {
		return ""
	}
	base = strings.TrimRight(base, "/")
	for _, suffix := range []string{"/chat/completions", "/messages", "/responses"} {
		if strings.HasSuffix(base, suffix) {
			base = strings.TrimSuffix(base, suffix)
			break
		}
	}
	return strings.TrimRight(base, "/")
}

// fetchModelList 请求单个候选 URL 并解析模型名。
func (p *OpenAICompat) fetchModelList(ctx context.Context, url string) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, WrapError(err, ErrKindBadRequest, 0, "构造请求失败")
	}
	req.Header.Set("Accept", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
		// Anthropic 兼容端点要求 x-api-key + 版本头
		if strings.Contains(strings.ToLower(p.baseURL), "anthropic") {
			req.Header.Set("x-api-key", p.apiKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	}
	for k, v := range p.extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, WrapError(err, ErrKindNetwork, 0, "发起请求失败")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, classifyStatus(resp)
	}
	// 模型清单通常几十~几百 KB，限 8MB 防御异常响应
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, WrapError(err, ErrKindNetwork, 0, "读取响应失败")
	}
	list, err := parseModelList(raw)
	if err != nil {
		return nil, WrapError(err, ErrKindBadRequest, resp.StatusCode, "解析模型列表失败")
	}
	if len(list) == 0 {
		return nil, NewError(ErrKindBadRequest, resp.StatusCode, "上游返回的模型列表为空")
	}
	return list, nil
}

// parseModelList 解析 OpenAI / Ollama 两种模型清单形状（去重保序）。
func parseModelList(raw []byte) ([]string, error) {
	var openai struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	var ollama struct {
		Models []struct {
			Name string `json:"name"`
			// Ollama 新版也返回 model 字段
			Model string `json:"model"`
		} `json:"models"`
	}
	// 先按 OpenAI 形状解析，无结果再按 Ollama 形状
	if err := json.Unmarshal(raw, &openai); err == nil && len(openai.Data) > 0 {
		out := make([]string, 0, len(openai.Data))
		for _, m := range openai.Data {
			if id := strings.TrimSpace(m.ID); id != "" {
				out = append(out, id)
			}
		}
		return dedupeStrings(out), nil
	}
	if err := json.Unmarshal(raw, &ollama); err == nil && len(ollama.Models) > 0 {
		out := make([]string, 0, len(ollama.Models))
		for _, m := range ollama.Models {
			name := strings.TrimSpace(m.Name)
			if name == "" {
				name = strings.TrimSpace(m.Model)
			}
			if name != "" {
				out = append(out, name)
			}
		}
		return dedupeStrings(out), nil
	}
	// 两种形状都不匹配：返回解析错误而非空列表，避免「静默成功但无模型」
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	return nil, nil
}

// dedupeStrings 去重并保持首次出现顺序。
func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// summarizeModelFetch 把单个候选的失败原因压缩成一句话（用于聚合错误）。
func summarizeModelFetch(err error) string {
	le := AsError(err)
	if le == nil {
		return "未知错误"
	}
	if le.StatusCode > 0 {
		return fmt.Sprintf("HTTP %d %s", le.StatusCode, le.Message)
	}
	return le.Message
}

// ChatStream 发起流式调用（含可重试错误的指数退避）。
func (p *OpenAICompat) ChatStream(ctx context.Context, req ChatRequest) (Stream, error) {
	body, err := p.buildBody(req)
	if err != nil {
		return nil, err
	}
	resp, err := p.doWithRetry(ctx, body)
	if err != nil {
		return nil, err
	}
	return newOpenAIStream(resp, p.firstTokenTimeout), nil
}

// ============ 请求构建 ============

type oaFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaToolCall struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"`
	Function oaFunction `json:"function"`
}

type oaMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content"`
	Name       string       `json:"name,omitempty"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type oaTool struct {
	Type     string    `json:"type"`
	Function oaToolDef `json:"function"`
}

func (p *OpenAICompat) buildBody(req ChatRequest) ([]byte, error) {
	msgs := make([]oaMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, oaMessage{Role: string(RoleSystem), Content: req.System})
	}
	for _, m := range req.Messages {
		om := oaMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			om.ToolCalls = append(om.ToolCalls, oaToolCall{
				ID:       tc.ID,
				Type:     "function",
				Function: oaFunction{Name: tc.Name, Arguments: string(tc.Arguments)},
			})
		}
		msgs = append(msgs, om)
	}

	payload := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   req.Stream,
	}
	if req.Stream {
		// 流式必须显式要求 usage，否则拿不到真实 token 数（docs/需求说明文档.md §7.5）
		payload["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(req.Tools) > 0 {
		tools := make([]oaTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			schema := t.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, oaTool{
				Type:     "function",
				Function: oaToolDef{Name: t.Name, Description: t.Description, Parameters: schema},
			})
		}
		payload["tools"] = tools
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	for k, v := range req.ExtraBody {
		payload[k] = v
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, NewError(ErrKindInvalidPayload, 0, "序列化请求失败: %v", err)
	}
	return raw, nil
}

// ============ 请求发送与重试 ============

func (p *OpenAICompat) doWithRetry(ctx context.Context, body []byte) (*http.Response, error) {
	var lastErr *Error
	for attempt := 0; attempt <= p.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, AsError(ctx.Err())
			case <-time.After(p.backoff(attempt, lastErr)):
			}
		}
		resp, err := p.send(ctx, body)
		if err != nil {
			le := AsError(err)
			if !le.Retryable() {
				return nil, le
			}
			lastErr = le
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		le := classifyStatus(resp)
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
		_ = resp.Body.Close()
		if !le.Retryable() {
			return nil, le
		}
		lastErr = le
	}
	if lastErr == nil {
		lastErr = NewError(ErrKindNetwork, 0, "请求失败")
	}
	return nil, lastErr
}

func (p *OpenAICompat) send(ctx context.Context, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, WrapError(err, ErrKindBadRequest, 0, "构造请求失败")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	// 会话路由头（对齐桌面端 llm.rs）：opencode zen 等网关强制要求，其余厂商忽略不碍事；
	// 放在 ExtraHeaders 之前，允许 ExtraHeaders 按厂商覆盖
	httpReq.Header.Set("x-opencode-session", p.sessionID)
	httpReq.Header.Set("x-session-id", p.sessionID)
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	for k, v := range p.extraHeaders {
		httpReq.Header.Set(k, v)
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, WrapError(err, ErrKindNetwork, 0, "发起请求失败")
	}
	return resp, nil
}

// backoff 指数退避 + 抖动；若上游给了 Retry-After 优先使用（上限 30s）。
func (p *OpenAICompat) backoff(attempt int, lastErr *Error) time.Duration {
	if lastErr != nil && lastErr.RetryAfter > 0 {
		d := time.Duration(lastErr.RetryAfter) * time.Second
		if d > 30*time.Second {
			d = 30 * time.Second
		}
		return d
	}
	d := p.baseDelay << (attempt - 1) // 500ms, 1s, 2s ...
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	// ±20% 抖动，避免多 Run 同时重试打爆上游
	jitter := time.Duration(rand.Int63n(int64(d)/5+1)) - d/10
	return d + jitter
}

// classifyStatus 把非 200 响应分类为可判定重试与否的错误。
func classifyStatus(resp *http.Response) *Error {
	status := resp.StatusCode
	retryAfter := 0
	if v := resp.Header.Get("Retry-After"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			retryAfter = n
		}
	}
	switch {
	case status == http.StatusTooManyRequests:
		e := NewError(ErrKindRateLimited, status, "上游限流")
		e.RetryAfter = retryAfter
		return e
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return NewError(ErrKindAuth, status, "鉴权失败，请检查 API Key")
	case status >= 500:
		e := NewError(ErrKindServerError, status, "上游服务错误")
		e.RetryAfter = retryAfter
		return e
	case status >= 400:
		return NewError(ErrKindBadRequest, status, "请求被上游拒绝")
	default:
		return NewError(ErrKindBadRequest, status, "非预期响应")
	}
}

// ============ SSE 流解析 ============

// oaStreamChunk 单个 SSE data 的 JSON 结构。
type oaStreamChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type openaiStream struct {
	resp      *http.Response
	scanner   *bufio.Scanner
	timer     *time.Timer
	timedOut  int32
	firstDone bool
	closed    bool
}

func newOpenAIStream(resp *http.Response, firstTokenTimeout time.Duration) *openaiStream {
	s := &openaiStream{resp: resp, scanner: bufio.NewScanner(resp.Body)}
	// SSE 单行可能较长（工具参数分片），放宽到 1MB
	s.scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	if firstTokenTimeout > 0 {
		s.timer = time.AfterFunc(firstTokenTimeout, func() {
			atomic.StoreInt32(&s.timedOut, 1)
			_ = resp.Body.Close() // 强制中断阻塞中的 Read
		})
	}
	return s
}

// Recv 读取下一个增量。正常结束返回 io.EOF。
func (s *openaiStream) Recv() (Chunk, error) {
	for {
		if s.closed {
			return Chunk{}, io.EOF
		}
		if !s.scanner.Scan() {
			err := s.scanner.Err()
			if !s.firstDone {
				s.stopTimer()
			}
			if err != nil {
				if atomic.LoadInt32(&s.timedOut) == 1 {
					return Chunk{}, NewError(ErrKindFirstToken, 0, "等待首 token 超时")
				}
				return Chunk{}, WrapError(err, ErrKindNetwork, 0, "读取流失败")
			}
			return Chunk{}, io.EOF
		}

		line := strings.TrimSpace(s.scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue // 空行 / 注释 / 心跳
		}
		if !strings.HasPrefix(line, "data:") {
			continue // event: / id: 等字段忽略
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			s.stopTimer()
			return Chunk{}, io.EOF
		}

		var raw oaStreamChunk
		if err := json.Unmarshal([]byte(data), &raw); err != nil {
			// 单条脏数据不应中断整条流
			continue
		}
		if raw.Error != nil {
			return Chunk{}, NewError(ErrKindServerError, 0, "上游返回错误: %s", raw.Error.Message)
		}

		s.markFirst()

		chunk := Chunk{}
		if raw.Usage != nil {
			u := &Usage{
				PromptTokens:     raw.Usage.PromptTokens,
				CompletionTokens: raw.Usage.CompletionTokens,
				TotalTokens:      raw.Usage.TotalTokens,
			}
			if raw.Usage.PromptTokensDetails != nil {
				u.CachedTokens = raw.Usage.PromptTokensDetails.CachedTokens
			}
			chunk.Usage = u
		}
		if len(raw.Choices) > 0 {
			c := raw.Choices[0]
			chunk.Delta = c.Delta.Content
			chunk.Reasoning = c.Delta.ReasoningContent
			if c.FinishReason != nil {
				chunk.FinishReason = *c.FinishReason
			}
			if len(c.Delta.ToolCalls) > 0 {
				tc := c.Delta.ToolCalls[0]
				chunk.ToolCall = &ToolCallDelta{
					Index:          tc.Index,
					ID:             tc.ID,
					Name:           tc.Function.Name,
					ArgumentsDelta: tc.Function.Arguments,
				}
			}
		}
		// 空 chunk（既无内容也无 usage）跳过，避免上层空转
		if chunk.Delta == "" && chunk.Reasoning == "" && chunk.ToolCall == nil &&
			chunk.Usage == nil && chunk.FinishReason == "" {
			continue
		}
		return chunk, nil
	}
}

func (s *openaiStream) markFirst() {
	if s.firstDone {
		return
	}
	s.firstDone = true
	s.stopTimer()
}

func (s *openaiStream) stopTimer() {
	if s.timer != nil {
		s.timer.Stop()
	}
}

// Close 关闭流（幂等）。
func (s *openaiStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.stopTimer()
	return s.resp.Body.Close()
}
