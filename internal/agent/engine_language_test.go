package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"harness-go/internal/llm"
	"harness-go/internal/model"
	"harness-go/internal/prompt"
)

// ============ 中文思考近端锚定（2026-11 落地） ============
//
// # 背景（用户实际反馈 + 代码实证）
//
// 用户反馈思考过程大量是英文，而老版本多为中文。根因不在"新版删了某条约束"
// （两版都只约束"与用户交流"的语言），而在于：
//  1. 思考语言从未被正面约束；
//  2. 稳定前缀里的语言条款离生成点太远，工具续轮（Step≥2）又没有用户输入，
//     模型在英文密度极高的工具上下文中容易漂回英文。
//
// 因此引擎在**可变尾段**（DynamicTail）追加一句中文思考锚定，紧贴本轮 user
// 消息注入。本文件把三条不变量锁死：
//
//	① 锚定必须出现在 DynamicTail 里（近场生效）；
//	② 锚定**绝不**能进 SystemBase（污染稳定前缀 = 击穿 KV 缓存）；
//	③ 工具续轮（无用户输入）也必须带锚定（否则「首轮中文、后续英文」）。

// TestReasoningAnchorGoesToDynamicTail 锚定必须走 DynamicTail 通道。
func TestReasoningAnchorGoesToDynamicTail(t *testing.T) {
	b := &capturingBuilder{res: okBuildResult()}
	provider := &fakeProvider{rounds: [][]llm.Chunk{{{Delta: "好"}, {Usage: &llm.Usage{TotalTokens: 1}}}}}
	eng, _, _, _ := newTestEngine(t, provider, func(d *Deps) { d.Builder = b })

	if err := eng.Execute(context.Background(), baseRunRequest()); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if !strings.Contains(b.last.DynamicTail, reasoningLanguageAnchor) {
		t.Fatalf("DynamicTail 必须包含中文思考锚定，实际: %q", b.last.DynamicTail)
	}
}

// TestReasoningAnchorNeverInSystemBase 锚定绝不进 SystemBase（前缀缓存红线）。
//
// 这条是防倒退的核心：把锚定"顺手"挪进 system 会更省 token，但会让每轮 system
// 变化 → provider 前缀缓存全废（老版本踩过同类坑，见 prompt/builder.go 注释）。
func TestReasoningAnchorNeverInSystemBase(t *testing.T) {
	b := &capturingBuilder{res: okBuildResult()}
	provider := &fakeProvider{rounds: [][]llm.Chunk{{{Delta: "好"}, {Usage: &llm.Usage{TotalTokens: 1}}}}}
	eng, _, _, _ := newTestEngine(t, provider, func(d *Deps) { d.Builder = b })

	req := baseRunRequest()
	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	// SystemBase 由调用方原样透传给 Builder（引擎不得篡改）
	if strings.Contains(b.last.SystemBase, reasoningLanguageAnchor) {
		t.Fatalf("锚定不得写入 SystemBase（会击穿前缀缓存）: %q", b.last.SystemBase)
	}
	if b.last.SystemBase != req.SystemBase {
		t.Fatalf("引擎不得篡改 SystemBase: %q", b.last.SystemBase)
	}
}

// TestReasoningAnchorPersistsAcrossToolRounds 工具续轮（无用户输入）仍带锚定。
//
// 这是用户实际感受最强的场景：第一个工具调用之后，模型往往连续思考十几步，
// 而这十几步没有任何用户输入，只有工具结果（英文 schema / 英文报错）——
// 若此处不带锚定，思考就会在这些步骤里成片转成英文。
func TestReasoningAnchorPersistsAcrossToolRounds(t *testing.T) {
	// 第 1 轮发起工具调用，第 2 轮给出最终答复（对应 Step 0 / Step 1）
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "read_file"}}},
		{{Delta: "看完了"}, {Usage: &llm.Usage{TotalTokens: 2}}},
	}}
	b := &capturingBuilder{res: okBuildResult()}
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK}}

	eng, sdb, _, _ := newTestEngine(t, provider, func(d *Deps) {
		d.Builder = b
		d.Tools = tools
		d.Catalog = readOnlyCatalog{}
	})

	if err := eng.Execute(context.Background(), baseRunRequest()); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if sdb.run.Status != model.RunCompleted {
		t.Fatalf("Run 应完成，得到 %s", sdb.run.Status)
	}
	// capturingBuilder 只保留最后一次 Build 输入 = 工具回灌后的续轮
	if b.last.UserInput != "" {
		t.Fatalf("续轮应无用户输入（否则本测试未覆盖目标场景）: %q", b.last.UserInput)
	}
	if !strings.Contains(b.last.DynamicTail, reasoningLanguageAnchor) {
		t.Fatalf("工具续轮必须仍带中文思考锚定，实际: %q", b.last.DynamicTail)
	}
}

// TestJoinTailSectionsSkipsEmpty 尾段拼接必须跳过空段（否则末轮会多出空行）。
func TestJoinTailSectionsSkipsEmpty(t *testing.T) {
	if got := joinTailSections("", "  ", "A"); got != "A" {
		t.Fatalf("空段应被跳过，得到 %q", got)
	}
	if got := joinTailSections("A", "B"); got != "A\n\nB" {
		t.Fatalf("相邻段应以空行分隔，得到 %q", got)
	}
	if got := joinTailSections(); got != "" {
		t.Fatalf("无段时应返回空串，得到 %q", got)
	}
}

// okBuildResult 构造一个不会触发降级/超预算的组装结果。
//
// 为什么需要：capturingBuilder 返回零值 BuildResult 时 OverBudget=false 恰好可用，
// 但显式给出非零 Total 更贴近真实路径，避免"靠零值偶然通过"。
func okBuildResult() prompt.BuildResult {
	return prompt.BuildResult{Breakdown: prompt.Breakdown{Total: 100}}
}

// TestReasoningAnchorReachesUserMessage 端到端验证：锚定真的进了发给模型的 user 消息。
//
// 为什么不止步于「DynamicTail 有锚定」：DynamicTail 只是组装输入，
// 若 Builder 侧逻辑变化（比如过滤/丢弃），锚定仍可能到不了模型。
// 这里用**真实 Builder + 捕获 provider 请求**，锁死"模型可见"这一最终事实。
func TestReasoningAnchorReachesUserMessage(t *testing.T) {
	var captured llm.ChatRequest
	provider := &capturingProvider{
		rounds: [][]llm.Chunk{{{Delta: "好"}, {Usage: &llm.Usage{TotalTokens: 1}}}},
		onReq:  func(r llm.ChatRequest) { captured = r },
	}
	// 不覆盖 Builder：用 newTestEngine 默认的真实 Builder
	eng, _, _, _ := newTestEngine(t, provider)

	if err := eng.Execute(context.Background(), baseRunRequest()); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}

	// system 必须干净
	if strings.Contains(captured.System, reasoningLanguageAnchor) {
		t.Fatalf("锚定泄漏进 system: %q", captured.System)
	}
	// 最后一条 user 消息必须包含锚定与用户输入
	if len(captured.Messages) == 0 {
		t.Fatal("应至少有一条消息")
	}
	last := captured.Messages[len(captured.Messages)-1]
	if last.Role != llm.RoleUser {
		t.Fatalf("最后一条应为 user，得到 %s", last.Role)
	}
	if !strings.Contains(last.Content, reasoningLanguageAnchor) {
		t.Fatalf("锚定必须进入发给模型的 user 消息，实际: %q", last.Content)
	}
	if !strings.Contains(last.Content, "你好") {
		t.Fatalf("用户输入不得被锚定挤掉: %q", last.Content)
	}
}

// capturingProvider 记录每次实际发给模型的请求（fakeProvider 只回放预设轮次，
// 不暴露请求体，无法验证"模型到底看到了什么"）。
type capturingProvider struct {
	rounds [][]llm.Chunk
	idx    int
	onReq  func(llm.ChatRequest)
	mu     sync.Mutex
}

func (p *capturingProvider) Name() string { return "capturing" }

func (p *capturingProvider) ChatStream(_ context.Context, req llm.ChatRequest) (llm.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.onReq != nil {
		p.onReq(req)
	}
	if p.idx >= len(p.rounds) {
		return nil, errors.New("capturingProvider: 没有更多预设轮次")
	}
	chunks := p.rounds[p.idx]
	p.idx++
	return &fakeStream{chunks: chunks}, nil
}
