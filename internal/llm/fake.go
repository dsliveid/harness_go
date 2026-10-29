package llm

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// FakeProvider 本地回显 provider（无 API Key 时的端到端验证用）。
//
// 行为：把最后一条 user 消息按字符切分成增量逐个返回，模拟真实流式输出。
// 用途：M0 出口验收「浏览器看到打字机效果」不依赖真实厂商额度。
type FakeProvider struct {
	// Delay 每个增量之间的间隔（默认 15ms，设为负数可关闭延迟）
	Delay time.Duration
	// Prefix 回复前缀（便于在 UI 上识别是假 provider）
	Prefix string
}

// NewFakeProvider 创建回显 provider。
func NewFakeProvider() *FakeProvider {
	return &FakeProvider{Delay: 15 * time.Millisecond, Prefix: "[fake] "}
}

// Name 实现 Provider。
func (p *FakeProvider) Name() string { return "fake" }

// ChatStream 实现 Provider。
func (p *FakeProvider) ChatStream(ctx context.Context, req ChatRequest) (Stream, error) {
	var lastUser string
	for _, m := range req.Messages {
		if m.Role == RoleUser {
			lastUser = m.Content
		}
	}
	reply := p.Prefix + "收到：" + lastUser

	// 按 rune 切分，避免拆坏 UTF-8
	runes := []rune(reply)
	chunks := make([]Chunk, 0, len(runes))
	for _, r := range runes {
		chunks = append(chunks, Chunk{Delta: string(r)})
	}
	chunks = append(chunks, Chunk{
		FinishReason: "stop",
		Usage: &Usage{
			PromptTokens:     estimatePromptTokens(req),
			CompletionTokens: len(runes),
			TotalTokens:      estimatePromptTokens(req) + len(runes),
			Estimated:        true,
		},
	})
	return &fakeStream{ctx: ctx, chunks: chunks, delay: p.Delay}, nil
}

type fakeStream struct {
	ctx    context.Context
	chunks []Chunk
	i      int
	delay  time.Duration
}

func (s *fakeStream) Recv() (Chunk, error) {
	if s.i >= len(s.chunks) {
		return Chunk{}, EOF
	}
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-s.ctx.Done():
			return Chunk{}, s.ctx.Err()
		}
	}
	c := s.chunks[s.i]
	s.i++
	return c, nil
}

func (s *fakeStream) Close() error { return nil }

// estimatePromptTokens 粗略估算（仅用于 fake provider 的 usage 展示）。
func estimatePromptTokens(req ChatRequest) int {
	n := len([]rune(req.System))
	for _, m := range req.Messages {
		n += len([]rune(m.Content))
	}
	if n == 0 {
		return 1
	}
	return n/4 + 1
}

// Describe 返回 fake provider 的说明（启动日志用）。
func (p *FakeProvider) Describe() string {
	return fmt.Sprintf("fake provider（回显模式，前缀 %q）", strings.TrimSpace(p.Prefix))
}
