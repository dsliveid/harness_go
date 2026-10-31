package service

// OpenAICompatService 实现 P1 OpenAI 兼容端点（需求 §4.3 / F9.2）：
//
//	GET /v1/models                 → OpenAI 模型清单（enabled providers 聚合）
//	POST /v1/chat/completions      → 非流式 / SSE 流式（stream:true）
//
// 会话映射（「内部映射到一个隐藏会话」）：
//   - 请求带 X-Session-ID → 复用该会话（只追加本轮 user 输入，历史以库为准）；
//   - 否则每请求创建**隐藏会话**（sessions.source='openai'，不出现在默认列表），
//     并把客户端带来的完整消息历史回放进新会话。
//
// 执行复用 RunService 全链路（记忆/SOP/动态工具/审批/预算闸门），本层只做协议形状
// 转换与同步等待；客户端 system 消息经 CreateRunWithSystem 覆盖本轮 SystemBase。
// AccessMode 沿用会话设置（隐藏会话默认 confirm——danger 写入仍需审批，
// 同步等待会阻塞到审批超时，属诚实行为）。

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/events"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// openaiPollInterval 同步等待 Run 终态的轮询间隔（DB 为准）。
const openaiPollInterval = 40 * time.Millisecond

// OpenAIModel /v1/models 条目。
type OpenAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// OpenAIMessage 客户端消息（OpenAI chat 协议形状，只取所需字段）。
type OpenAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// OpenAIChatRequest /v1/chat/completions 请求（只取所需字段，其余忽略）。
type OpenAIChatRequest struct {
	Model    string          `json:"model"`
	Messages []OpenAIMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

// OpenAIUsage token 用量。
type OpenAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// OpenAIChoice 单条 choice。
type OpenAIChoice struct {
	Index   int `json:"index"`
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}

// OpenAIChatResponse 非流式响应。
type OpenAIChatResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage"`
}

// OpenAIStreamItem 流式回调载荷（handler 负责 SSE 编码）。
type OpenAIStreamItem struct {
	// Delta 本帧增量文本。
	Delta string
	// FinishReason 非空 = 终止帧（"stop"）。
	FinishReason string
	// Usage 终止帧可带（include_usage 语义由 handler 决定）。
	Usage *OpenAIUsage
	// Err 非空 = 本流以错误终止（handler 输出 OpenAI error 帧）。
	Err error
}

// OpenAICompatServiceOptions 依赖。
type OpenAICompatServiceOptions struct {
	Main     *store.MainStore
	Sessions *store.SessionStore
	Runs     *RunService
	Bus      *events.Bus
}

// OpenAICompatService OpenAI 兼容端点用例。
type OpenAICompatService struct {
	main     *store.MainStore
	sessions *store.SessionStore
	runs     *RunService
	bus      *events.Bus
}

// NewOpenAICompatService 创建用例。
func NewOpenAICompatService(opts OpenAICompatServiceOptions) *OpenAICompatService {
	return &OpenAICompatService{
		main:     opts.Main,
		sessions: opts.Sessions,
		runs:     opts.Runs,
		bus:      opts.Bus,
	}
}

// Models 聚合 enabled providers 的模型清单 + 内置 default。
func (s *OpenAICompatService) Models(ctx context.Context) ([]OpenAIModel, error) {
	providers, err := s.main.Providers().List(ctx)
	if err != nil {
		return nil, wrapStoreErr(err, apperr.CodeInternal, "读取 providers 失败")
	}
	created := time.Now().Unix()
	out := []OpenAIModel{{
		ID: "default", Object: "model", Created: created, OwnedBy: "harness-go",
	}}
	seen := map[string]struct{}{"default": {}}
	for _, p := range providers {
		if !p.Enabled {
			continue
		}
		for _, m := range p.Models {
			if m == "" {
				continue
			}
			if _, dup := seen[m]; dup {
				continue
			}
			seen[m] = struct{}{}
			out = append(out, OpenAIModel{ID: m, Object: "model", Created: created, OwnedBy: p.Name})
		}
	}
	return out, nil
}

// resolveSession 会话映射：显式 ID 复用（历史以库为准），否则创建隐藏会话并回放历史。
// 返回（会话 ID，本轮 user 输入，客户端 system，错误）。
func (s *OpenAICompatService) resolveSession(ctx context.Context, req *OpenAIChatRequest, sessionID string) (string, string, string, error) {
	lastUser, lastUserIdx := "", -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastUser, lastUserIdx = req.Messages[i].Content, i
			break
		}
	}
	if strings.TrimSpace(lastUser) == "" {
		return "", "", "", apperr.New(apperr.CodeInvalidArgument, "messages 中必须至少包含一条 user 消息")
	}

	if sid := strings.TrimSpace(sessionID); sid != "" {
		// 复用显式会话：历史以库为准，只追加本轮输入（客户端重复携带全量历史不回放）
		if _, err := s.main.Sessions().Get(ctx, sid); err != nil {
			return "", "", "", wrapStoreErr(err, apperr.CodeSessionNotFound, "X-Session-ID 会话不存在")
		}
		return sid, lastUser, lastSystem(req.Messages), nil
	}

	// 隐藏会话（source=openai）：回放客户端全量历史
	sess := &model.Session{
		ID:     "sess-" + newID(),
		Title:  "OpenAI 接入 " + time.Now().Format("0102-150405"),
		Source: "openai",
	}
	if err := s.main.Sessions().Create(ctx, sess); err != nil {
		return "", "", "", wrapStoreErr(err, apperr.CodeInternal, "创建接入会话失败")
	}
	sdb, err := s.sessions.Acquire(ctx, sess.ID)
	if err != nil {
		return "", "", "", err
	}
	system := ""
	for i, m := range req.Messages {
		switch m.Role {
		case "system":
			if system == "" {
				system = m.Content
			}
		case "user", "assistant":
			if i == lastUserIdx {
				continue // 本轮输入交给引擎（避免重复入库）
			}
			role := model.RoleUser
			if m.Role == "assistant" {
				role = model.RoleAssistant
			}
			if err := sdb.Messages().Append(ctx, &model.Message{
				ID: "m-" + newID(), Role: role, Content: m.Content,
			}); err != nil {
				return "", "", "", wrapStoreErr(err, apperr.CodeInternal, "回放历史失败")
			}
		default:
			// tool/function 等角色在无 tool_call 链的隐藏会话里无法成立，诚实跳过
		}
	}
	return sess.ID, lastUser, system, nil
}

// Chat 非流式：同步执行到终态，返回 OpenAI 形状响应。
func (s *OpenAICompatService) Chat(ctx context.Context, req OpenAIChatRequest, sessionID string) (*OpenAIChatResponse, error) {
	sid, input, system, err := s.resolveSession(ctx, &req, sessionID)
	if err != nil {
		return nil, err
	}
	run, err := s.runs.CreateRunWithSystem(ctx, sid, input, req.Model, "", system)
	if err != nil {
		return nil, err
	}
	final, err := s.awaitTerminal(ctx, sid, run.ID)
	if err != nil {
		return nil, err
	}
	if final.Status != model.RunCompleted {
		return nil, apperr.New(apperr.CodeInternal, "Run 终态 %s: %s", final.Status, final.ErrorMessage)
	}
	content, err := s.lastAssistant(ctx, sid)
	if err != nil {
		return nil, err
	}
	resp := &OpenAIChatResponse{
		ID: "chatcmpl-" + run.ID, Object: "chat.completion",
		Created: final.CreatedAt / 1000, Model: req.Model,
	}
	choice := OpenAIChoice{Index: 0, FinishReason: "stop"}
	choice.Message.Role = "assistant"
	choice.Message.Content = content
	resp.Choices = append(resp.Choices, choice)
	resp.Usage = OpenAIUsage{
		PromptTokens:     final.InputTokens,
		CompletionTokens: final.OutputTokens,
		TotalTokens:      final.InputTokens + final.OutputTokens,
	}
	return resp, nil
}

// ChatStream 流式：订阅 Run 事件转发 message.delta，终态后回调终止帧（含 usage）。
func (s *OpenAICompatService) ChatStream(ctx context.Context, req OpenAIChatRequest, sessionID string, emit func(OpenAIStreamItem) error) error {
	sid, input, system, err := s.resolveSession(ctx, &req, sessionID)
	if err != nil {
		return err
	}
	run, err := s.runs.CreateRunWithSystem(ctx, sid, input, req.Model, "", system)
	if err != nil {
		return err
	}
	// 订阅（fromSeq=0 从头补发，规避「先创建后订阅」的启动竞态——
	// Run 前几条事件先落环形缓冲，订阅时回放）
	sub, err := s.bus.Subscribe(run.ID, 0)
	if err != nil {
		return err
	}
	defer s.bus.Unsubscribe(sub)

	var streamErr error
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, open := <-sub.C():
			if !open {
				if streamErr != nil {
					_ = emit(OpenAIStreamItem{Err: streamErr})
					return streamErr
				}
				// 事件源关闭但未见终态（异常路径）：以 DB 为准兜底
				final, gErr := s.runs.GetRun(ctx, sid, run.ID)
				if gErr != nil || final.Status != model.RunCompleted {
					return apperr.New(apperr.CodeInternal, "Run 事件源提前关闭")
				}
				return s.emitFinish(ctx, sid, run.ID, emit)
			}
			switch ev.Type {
			case events.TypeMessageDelta:
				var p struct {
					Delta string `json:"delta"`
				}
				if json.Unmarshal(ev.Payload, &p) == nil && p.Delta != "" {
					if err := emit(OpenAIStreamItem{Delta: p.Delta}); err != nil {
						return err // 客户端断开
					}
				}
			case events.TypeRunStatus:
				var p struct {
					Status string `json:"status"`
				}
				if json.Unmarshal(ev.Payload, &p) != nil {
					continue
				}
				st := model.RunStatus(p.Status)
				if !st.IsTerminal() {
					continue
				}
				if st != model.RunCompleted {
					var p2 struct {
						ErrorMessage string `json:"error_message"`
					}
					_ = json.Unmarshal(ev.Payload, &p2)
					streamErr = apperr.New(apperr.CodeInternal, "Run 终态 %s: %s", st, p2.ErrorMessage)
					continue // 等待事件源关闭（终态后引擎关源）再统一收尾
				}
				return s.emitFinish(ctx, sid, run.ID, emit)
			}
		}
	}
}

// emitFinish 终止帧（usage 以 DB 为准）。
func (s *OpenAICompatService) emitFinish(ctx context.Context, sessionID, runID string, emit func(OpenAIStreamItem) error) error {
	final, err := s.runs.GetRun(ctx, sessionID, runID)
	if err != nil {
		return err
	}
	usage := OpenAIUsage{
		PromptTokens:     final.InputTokens,
		CompletionTokens: final.OutputTokens,
		TotalTokens:      final.InputTokens + final.OutputTokens,
	}
	return emit(OpenAIStreamItem{FinishReason: "stop", Usage: &usage})
}

// awaitTerminal 轮询 Run 至终态（DB 为准；ctx 取消即中断等待）。
func (s *OpenAICompatService) awaitTerminal(ctx context.Context, sessionID, runID string) (*model.Run, error) {
	for {
		run, err := s.runs.GetRun(ctx, sessionID, runID)
		if err != nil {
			return nil, err
		}
		if run.Status.IsTerminal() {
			return run, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(openaiPollInterval):
		}
	}
}

// lastAssistant 读取本轮最终 assistant 消息（seq 最大的 assistant）。
func (s *OpenAICompatService) lastAssistant(ctx context.Context, sessionID string) (string, error) {
	sdb, err := s.sessions.Acquire(ctx, sessionID)
	if err != nil {
		return "", err
	}
	msgs, err := sdb.Messages().ListVisible(ctx, 0, 500)
	if err != nil {
		return "", wrapStoreErr(err, apperr.CodeInternal, "读取消息失败")
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == model.RoleAssistant {
			return msgs[i].Content, nil
		}
	}
	return "", apperr.New(apperr.CodeInternal, "Run 完成但没有 assistant 消息")
}

func lastSystem(msgs []OpenAIMessage) string {
	for _, m := range msgs {
		if m.Role == "system" {
			return m.Content
		}
	}
	return ""
}
