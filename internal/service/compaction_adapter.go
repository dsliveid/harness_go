package service

// 上下文压缩的装配层（把 compact 包接到 agent.Compactor 端口上）。
//
// # 为什么需要这层适配
//
//   - compact 包刻意不依赖 agent（否则 agent → compact → agent 循环依赖）；
//   - agent 也刻意不依赖 compact（它是内核，只认端口）。
//
// 因此由 service 层做双向适配：这是唯一同时可见两侧类型的地方。
// 这与 BudgetGate 放在 service 的理由完全一致（缺口清单 §2 缺口 1 的落位方案）。
//
// # 为什么按会话动态构造 Service
//
// 本项目存储是「每会话一个独立 SQLite 库」，压缩表也在会话库内。
// 但 compact.Service 被设计为**无状态**（好并发、好测试）。
// 二者结合的解法：CompactionAdapter 持有会话库与摘要器，每次调用按
// SessionID 现场组装一个绑定该会话的 compact.Service。
//
// 这样既保持了 compact 包与存储布局无关，又天然实现了会话隔离。

import (
	"context"
	"fmt"
	"io"
	"strings"

	"harness-go/internal/agent"
	"harness-go/internal/compact"
	"harness-go/internal/llm"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// CompactionAdapter 把 compact 包适配为 agent.Compactor。
type CompactionAdapter struct {
	// Sessions 会话库（按 sessionID 定位压缩表所在库）。
	Sessions *store.SessionStore
	// SummarizerFor 返回摘要器。入参为会话绑定的 model_ref（由引擎提供）。
	SummarizerFor func(ctx context.Context, modelRef string) compact.Summarizer
	// Now 时钟注入（测试用）；nil 时用 time.Now。
	Now func() int64
}

// 编译期断言：适配器必须满足 agent 端口。
var _ agent.Compactor = (*CompactionAdapter)(nil)

// NeedsCompaction 实现 agent.Compactor：委托 compact.Evaluate。
//
// 复用 compact.Evaluate 而不是在这里重写一遍阈值判断，是刻意为之：
// 水位的唯一真源必须在 compact 包（那里有单测锁定 80% 边界）。
// 若两处各写一份，将来调阈值必然漂移——这正是"同一规则两个实现"的经典腐化点。
func (a *CompactionAdapter) NeedsCompaction(used, limit int) bool {
	return compact.Evaluate(used, limit).NeedsCompaction
}

// service 为指定会话构造一个绑定其存储的压缩服务。
func (a *CompactionAdapter) service(ctx context.Context, sessionID, modelRef string) (*compact.Service, error) {
	if a.Sessions == nil {
		return nil, fmt.Errorf("service: 压缩适配器缺少会话库")
	}
	st := newSessionCompactionStore(a.Sessions, sessionID)
	sm := a.SummarizerFor(ctx, modelRef)
	if sm == nil {
		return nil, fmt.Errorf("service: 无可用摘要器")
	}
	return compact.New(compact.Options{
		Store:      st,
		Summarizer: sm,
		Now:        a.Now,
	})
}

// Compact 实现 agent.Compactor。
func (a *CompactionAdapter) Compact(ctx context.Context, req agent.CompactRequest) (agent.CompactResult, error) {
	svc, err := a.service(ctx, req.SessionID, req.ModelRef)
	if err != nil {
		// 装配失败不是致命错误：压缩是优化项，退回原样继续跑。
		return agent.CompactResult{}, nil
	}
	res, cerr := svc.CompactIfNeeded(ctx, req.Used, req.Limit, req.Messages, req.Running)
	// ErrSummaryTooLong 是「本次不值得压」，对引擎而言等同于未压缩，
	// 不应作为错误上报——否则引擎会记录一条无意义的告警。
	if cerr != nil && !isNoopCompactionError(cerr) {
		return agent.CompactResult{}, cerr
	}
	if !res.Compacted {
		return agent.CompactResult{}, nil
	}
	return agent.CompactResult{
		Compacted:        true,
		StartSeq:         res.Record.StartSeq,
		EndSeq:           res.Record.EndSeq,
		ReplacedMessages: res.Record.ReplacedMessages,
		SavedTokens:      res.Record.ReplacedTokens,
		Summary:          res.Record.SummaryMarkdown,
	}, nil
}

// History 实现 agent.Compactor：返回叠加摘要后的历史。
func (a *CompactionAdapter) History(ctx context.Context, sessionID string, msgs []model.Message) ([]llm.Message, error) {
	svc, err := a.service(ctx, sessionID, "")
	if err != nil {
		// 无法构造服务时退回未压缩历史：宁可多花 token，也不能让会话卡死。
		return compact.History(msgs, nil), nil
	}
	return svc.LoadHistory(ctx, msgs)
}

// isNoopCompactionError 判断「压缩没做成但不能算失败」的错误。
func isNoopCompactionError(err error) bool {
	// 用 Error() 比较而非 errors.Is：compact 包不应把内部哨兵错误
	// 暴露成跨包契约（那会让两个包的演进互相牵制）。
	return strings.Contains(err.Error(), "摘要未短于原区间")
}

// LLMSummarizer 用当前会话的模型实现 compact.Summarizer。
type LLMSummarizer struct {
	Provider llm.Provider
	Model    string
	// MaxTokens 摘要输出上限（摘要本身就该短，硬限避免模型长篇大论）。
	MaxTokens int
}

// 编译期断言：必须满足 compact.Summarizer。
var _ compact.Summarizer = (*LLMSummarizer)(nil)

// Summarize 实现 compact.Summarizer。
//
// 只取正文增量，忽略 reasoning：思考过程对摘要没有价值，却会占满输出预算。
func (s *LLMSummarizer) Summarize(ctx context.Context, prompt string) (string, error) {
	if s.Provider == nil {
		return "", fmt.Errorf("service: 摘要器缺少 provider")
	}
	maxTokens := s.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 2048
	}
	stream, err := s.Provider.ChatStream(ctx, llm.ChatRequest{
		Model: s.Model,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: prompt},
		},
		MaxTokens: maxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("service: 摘要请求失败: %w", err)
	}
	defer stream.Close()

	var b strings.Builder
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			// 已有部分内容时返回已有内容：残摘要仍可能有用，
			// 且总比让整次压缩失败要好（压缩是优化项）。
			if b.Len() > 0 {
				return b.String(), nil
			}
			return "", fmt.Errorf("service: 摘要流读取失败: %w", err)
		}
		if chunk.Delta != "" {
			b.WriteString(chunk.Delta)
		}
	}
	return b.String(), nil
}
