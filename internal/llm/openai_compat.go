package llm

import (
	"bufio"
	"bytes"
	"context"
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
	ExtraHeaders      map[string]string // 额外请求头（如厂商要求的版本头）
}

// OpenAICompat OpenAI Chat Completions 协议适配器（流式）。
type OpenAICompat struct {
	name              string
	endpoint          string
	apiKey            string
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
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	return &OpenAICompat{
		name:              name,
		endpoint:          strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions",
		apiKey:            cfg.APIKey,
		client:            client,
		maxRetries:        cfg.MaxRetries,
		baseDelay:         cfg.BaseDelay,
		firstTokenTimeout: cfg.FirstTokenTimeout,
		extraHeaders:      cfg.ExtraHeaders,
	}, nil
}

// Name 返回适配器名。
func (p *OpenAICompat) Name() string { return p.name }

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
