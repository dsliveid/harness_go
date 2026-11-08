package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"harness-go/internal/approval"
	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/model"
	"harness-go/internal/prompt"
	"harness-go/internal/tokenizer"
)

// ============ fake 依赖 ============

type fakeSessionDB struct {
	mu         sync.Mutex
	run        *model.Run
	messages   []*model.Message
	toolEvents []*model.ToolEvent
	rules      []approval.Rule
	seq        int64
	usageP     int64
	usageC     int64
}

func (f *fakeSessionDB) AddRunUsage(_ context.Context, _ string, promptTokens, completionTokens, _ int64, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usageP = promptTokens
	f.usageC = completionTokens
	return nil
}

func (f *fakeSessionDB) AppendMessage(_ context.Context, m *model.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	m.Seq = f.seq
	f.messages = append(f.messages, m)
	return nil
}

func (f *fakeSessionDB) ListMessages(_ context.Context, afterSeq int64, _ int) ([]model.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.Message, 0, len(f.messages))
	for _, m := range f.messages {
		if m.Seq > afterSeq {
			out = append(out, *m)
		}
	}
	return out, nil
}

func (f *fakeSessionDB) GetRun(_ context.Context, _ string) (*model.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *f.run
	return &cp, nil
}

func (f *fakeSessionDB) TransitionRun(_ context.Context, _ string, to model.RunStatus, stopReason, errCode, errMsg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := CheckTransition(f.run.Status, to); err != nil {
		return err
	}
	f.run.Status = to
	f.run.StopReason = stopReason
	f.run.ErrorCode = errCode
	f.run.ErrorMessage = errMsg
	return nil
}

func (f *fakeSessionDB) InsertToolEvent(_ context.Context, ev *model.ToolEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *ev
	f.toolEvents = append(f.toolEvents, &cp)
	return nil
}

func (f *fakeSessionDB) FinishToolEvent(_ context.Context, id string, status model.ToolEventStatus, resultJSON, blob, errMsg string, exec ToolEventExec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ev := range f.toolEvents {
		if ev.ID == id {
			ev.Status = status
			ev.ResultJSON = json.RawMessage(resultJSON)
			ev.ResultBlob = blob
			ev.Error = errMsg
			e := exec.Normalized()
			ev.Executor = e.Executor
			ev.DeviceID = e.DeviceID
			ev.SourceTrust = e.SourceTrust
			return nil
		}
	}
	return errors.New("tool event not found")
}

func (f *fakeSessionDB) ApprovalRules(context.Context) ([]approval.Rule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]approval.Rule(nil), f.rules...), nil
}

func (f *fakeSessionDB) AddApprovalRule(_ context.Context, toolName, pattern string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, approval.Rule{ToolName: toolName, Pattern: pattern})
	return nil
}

type fakeProvider struct {
	rounds [][]llm.Chunk
	err    error
	idx    int
	mu     sync.Mutex
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) ChatStream(context.Context, llm.ChatRequest) (llm.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	if p.idx >= len(p.rounds) {
		return nil, errors.New("fakeProvider: 没有更多预设轮次")
	}
	chunks := p.rounds[p.idx]
	p.idx++
	return &fakeStream{chunks: chunks}, nil
}

type fakeStream struct {
	chunks []llm.Chunk
	i      int
}

func (s *fakeStream) Recv() (llm.Chunk, error) {
	if s.i >= len(s.chunks) {
		return llm.Chunk{}, io.EOF
	}
	c := s.chunks[s.i]
	s.i++
	return c, nil
}

func (s *fakeStream) Close() error { return nil }

type fakeResolver struct{ p llm.Provider }

func (r *fakeResolver) Resolve(context.Context, string) (llm.Provider, string, error) {
	return r.p, "test-model", nil
}

type fakeSummary struct {
	mu     sync.Mutex
	tokens int64
	status model.RunStatus
	touch  int
}

func (s *fakeSummary) TouchMessage(context.Context, string, int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touch++
	return nil
}

func (s *fakeSummary) AddTokensAndStatus(_ context.Context, _ string, tokens int64, status model.RunStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = tokens
	s.status = status
	return nil
}

type fakeTools struct {
	result ToolRunResult
	err    error
	calls  int
}

func (f *fakeTools) Run(context.Context, ToolRunRequest) (ToolRunResult, error) {
	f.calls++
	return f.result, f.err
}

func newTestEngine(t *testing.T, provider llm.Provider, opts ...func(*Deps)) (*Engine, *fakeSessionDB, *events.Bus, *fakeSummary) {
	t.Helper()
	sdb := &fakeSessionDB{run: &model.Run{ID: "run-1", Status: model.RunPending}}
	bus := events.NewBus(1000, 256)
	summary := &fakeSummary{}
	deps := Deps{
		Sessions: &fakeSessionProvider{db: sdb},
		Summary:  summary,
		Events:   bus,
		Models:   &fakeResolver{p: provider},
		Builder:  prompt.NewBuilder(tokenizer.NewHeuristic(), prompt.DefaultLimits()),
		Gate:     approval.NewGate(time.Second),
		MaxSteps: 5,
	}
	for _, o := range opts {
		o(&deps)
	}
	eng, err := New(deps)
	if err != nil {
		t.Fatalf("创建引擎失败: %v", err)
	}
	return eng, sdb, bus, summary
}

type fakeSessionProvider struct{ db *fakeSessionDB }

func (p *fakeSessionProvider) Session(context.Context, string) (SessionDB, error) { return p.db, nil }

func baseRunRequest() RunRequest {
	return RunRequest{
		RunID:      "run-1",
		SessionID:  "sess-1",
		UserInput:  "你好",
		ModelRef:   "p/m",
		AccessMode: approval.ModeFull, // full 模式：write 自动放行，测试聚焦主循环
		SystemBase: "你是助手",
	}
}

// ============ 测试 ============

// 正常完成：状态 pending→running→completed，消息落库，事件含 delta 与 end。
func TestEngineCompletesSimpleRun(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{{
		{Delta: "你"},
		{Delta: "好"},
		{Usage: &llm.Usage{TotalTokens: 12}},
	}}}
	eng, sdb, bus, summary := newTestEngine(t, provider)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := eng.Execute(ctx, baseRunRequest()); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}

	if sdb.run.Status != model.RunCompleted {
		t.Fatalf("Run 应为 completed，得到 %s", sdb.run.Status)
	}
	// assistant 消息落库且内容为拼接后的完整文本（DB 才是权威）
	if len(sdb.messages) != 1 {
		t.Fatalf("应落 1 条消息，得到 %d", len(sdb.messages))
	}
	if sdb.messages[0].Content != "你好" {
		t.Fatalf("消息内容错误: %q", sdb.messages[0].Content)
	}
	if sdb.messages[0].Role != model.RoleAssistant {
		t.Fatalf("角色错误: %s", sdb.messages[0].Role)
	}

	// 摘要回写（最终一致）
	if summary.status != model.RunCompleted || summary.tokens != 12 {
		t.Fatalf("摘要回写错误: status=%s tokens=%d", summary.status, summary.tokens)
	}

	// 事件序列：run.status(running) → message.start → delta×2 → message.end → run.status(completed)
	sub, err := bus.Subscribe("run-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Unsubscribe(sub)
	if sub.CloseReason() != events.ReasonRunClosed {
		t.Fatalf("Run 结束后订阅应关闭，原因=%q", sub.CloseReason())
	}
}

// 上下文取消 → cancelled（不是 failed）。
func TestEngineCancelledByContext(t *testing.T) {
	// 用一个永不返回的 provider：靠 ctx 取消打断
	provider := &slowProvider{}
	eng, sdb, _, _ := newTestEngine(t, provider)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	// 用户主动停止 / 超时属于 cancelled，不是 failed —— 因此不返回 error，状态以 DB 为准
	if err := eng.Execute(ctx, baseRunRequest()); err != nil {
		t.Fatalf("cancelled 不应返回 error，得到: %v", err)
	}
	if sdb.run.Status != model.RunCancelled {
		t.Fatalf("Run 应为 cancelled，得到 %s", sdb.run.Status)
	}
}

// 终态 run.status 事件携带 usage（P1）：与 AddRunUsage 落库同源同值，前端用量以此出栈。
func TestEngineTerminalStatusCarriesUsage(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{{
		{Delta: "你"},
		{Delta: "好"},
		{FinishReason: "stop", Usage: &llm.Usage{PromptTokens: 33, CompletionTokens: 2, TotalTokens: 35}},
	}}}
	eng, sdb, bus, _ := newTestEngine(t, provider)

	sub, err := bus.Subscribe("run-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Unsubscribe(sub)

	if err := eng.Execute(context.Background(), baseRunRequest()); err != nil {
		t.Fatalf("Run 应完成: %v", err)
	}

	// 落库口径：AddRunUsage 收到 33/2
	if sdb.usageP != 33 || sdb.usageC != 2 {
		t.Fatalf("AddRunUsage 应收到 33/2，得到 %d/%d", sdb.usageP, sdb.usageC)
	}
	// 事件口径：最终 run.status completed 事件带 input_tokens/output_tokens
	var got map[string]any
	for {
		ev, ok := <-sub.C()
		if !ok {
			t.Fatal("事件流提前关闭，未收到终态 run.status")
		}
		if ev.Type == events.TypeRunStatus {
			var payload map[string]any
			if json.Unmarshal(ev.Payload, &payload) == nil && payload["status"] == string(model.RunCompleted) {
				got = payload
				break
			}
		}
	}
	if got["input_tokens"] != float64(33) || got["output_tokens"] != float64(2) {
		t.Fatalf("终态事件应携带 usage 33/2，得到 %v/%v", got["input_tokens"], got["output_tokens"])
	}
}

// 思考过程（reasoning_content）全链路不变量：
//  1. 每片非空 Reasoning 都发 message.reasoning 事件（payload 只带增量）；
//  2. 完整思考过程随 assistant 消息落库（Reasoning 字段）；
//  3. message.end 事件携带 reasoning 终值，供前端断线对齐；
//  4. reasoning 绝不混入正文 delta（正文只含 Delta 片段）。
func TestEnginePublishesAndPersistsReasoning(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{{
		{Reasoning: "先看目录"},
		{Delta: "答"},
		{Reasoning: "再改文件"},
		{Delta: "案"},
		{FinishReason: "stop"},
	}}}
	eng, sdb, bus, _ := newTestEngine(t, provider)

	sub, err := bus.Subscribe("run-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Unsubscribe(sub)

	if err := eng.Execute(context.Background(), baseRunRequest()); err != nil {
		t.Fatalf("Run 应完成: %v", err)
	}

	// 落库口径：正文与思考过程分别落库，互不污染
	if len(sdb.messages) != 1 {
		t.Fatalf("应落 1 条消息，得到 %d", len(sdb.messages))
	}
	msg := sdb.messages[0]
	if msg.Content != "答案" {
		t.Fatalf("正文应只含 delta 片段，得到 %q", msg.Content)
	}
	if msg.Reasoning != "先看目录再改文件" {
		t.Fatalf("思考过程落库错误: %q", msg.Reasoning)
	}

	// 事件口径：逐片 message.reasoning + message.end 携带终值
	var reasoningDeltas []string
	var endPayload map[string]any
	all := []events.Type{}
	for {
		ev, ok := <-sub.C()
		if !ok {
			break
		}
		all = append(all, ev.Type)
		switch ev.Type {
		case events.TypeMessageReasoning:
			var p struct {
				Delta string `json:"delta"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatalf("message.reasoning payload 解析失败: %v", err)
			}
			if p.Delta == "" {
				t.Fatal("message.reasoning 不应为空增量")
			}
			reasoningDeltas = append(reasoningDeltas, p.Delta)
		case events.TypeMessageEnd:
			if err := json.Unmarshal(ev.Payload, &endPayload); err != nil {
				t.Fatalf("message.end payload 解析失败: %v", err)
			}
		}
	}
	if len(reasoningDeltas) != 2 {
		t.Fatalf("应发 2 条 message.reasoning，得到 %d（%v）", len(reasoningDeltas), reasoningDeltas)
	}
	if endPayload["reasoning"] != "先看目录再改文件" {
		t.Fatalf("message.end 应携带 reasoning 终值，得到 %v", endPayload["reasoning"])
	}
	if endPayload["content"] != "答案" {
		t.Fatalf("message.end content 应只含正文，得到 %v", endPayload["content"])
	}
	// 事件类型必须包含新增的 reasoning 事件（保证前端可订阅）
	found := false
	for _, ty := range all {
		if ty == events.TypeMessageReasoning {
			found = true
		}
	}
	if !found {
		t.Fatal("事件序列缺少 message.reasoning")
	}
}

// 无思考过程时 message.reasoning 一次都不发，message.end 的 reasoning 为空串（不伪造）。
func TestEngineNoReasoningEventWhenProviderSilent(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{{
		{Delta: "直接答"},
		{FinishReason: "stop"},
	}}}
	eng, _, bus, _ := newTestEngine(t, provider)

	sub, err := bus.Subscribe("run-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Unsubscribe(sub)

	if err := eng.Execute(context.Background(), baseRunRequest()); err != nil {
		t.Fatalf("Run 应完成: %v", err)
	}

	var endPayload map[string]any
	for ev := range sub.C() {
		if ev.Type == events.TypeMessageReasoning {
			t.Fatal("模型未返回思考过程时不得发 message.reasoning")
		}
		if ev.Type == events.TypeMessageEnd {
			_ = json.Unmarshal(ev.Payload, &endPayload)
		}
	}
	if endPayload == nil {
		t.Fatal("未收到 message.end")
	}
	if endPayload["reasoning"] != "" {
		t.Fatalf("无思考过程时 reasoning 应为空串，得到 %v", endPayload["reasoning"])
	}
}

type slowProvider struct{}

func (slowProvider) Name() string { return "slow" }

func (slowProvider) ChatStream(ctx context.Context, _ llm.ChatRequest) (llm.Stream, error) {
	return &slowStream{ctx: ctx}, nil
}

type slowStream struct{ ctx context.Context }

func (s *slowStream) Recv() (llm.Chunk, error) {
	<-s.ctx.Done()
	return llm.Chunk{}, s.ctx.Err()
}

func (s *slowStream) Close() error { return nil }

// 模型鉴权失败 → failed + error_code=auth（不可重试错误直接终态）。
func TestEngineProviderErrorMarksFailed(t *testing.T) {
	provider := &fakeProvider{err: llm.NewError(llm.ErrKindAuth, 401, "API Key 无效")}
	eng, sdb, _, _ := newTestEngine(t, provider)

	err := eng.Execute(context.Background(), baseRunRequest())
	if err == nil {
		t.Fatal("应返回错误")
	}
	if sdb.run.Status != model.RunFailed {
		t.Fatalf("Run 应为 failed，得到 %s", sdb.run.Status)
	}
	if sdb.run.ErrorCode != string(llm.ErrKindAuth) {
		t.Fatalf("error_code 应为 auth，得到 %q", sdb.run.ErrorCode)
	}
}

// 工具调用闭环：第一轮出 tool_call，执行后回灌，第二轮完成。
func TestEngineToolCallRoundTrip(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{
			{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "echo"}},
			{ToolCall: &llm.ToolCallDelta{Index: 0, ArgumentsDelta: `{"msg":"hi"}`}},
		},
		{
			{Delta: "工具已执行"},
		},
	}}
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK, ResultJSON: `{"ok":true}`}}
	eng, sdb, _, _ := newTestEngine(t, provider, func(d *Deps) { d.Tools = tools })

	if err := eng.Execute(context.Background(), baseRunRequest()); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if sdb.run.Status != model.RunCompleted {
		t.Fatalf("Run 应为 completed，得到 %s", sdb.run.Status)
	}
	if tools.calls != 1 {
		t.Fatalf("工具应执行 1 次，实际 %d", tools.calls)
	}
	// tool_events 落库且终态为 ok
	if len(sdb.toolEvents) != 1 {
		t.Fatalf("应有 1 条工具事件，得到 %d", len(sdb.toolEvents))
	}
	if sdb.toolEvents[0].Status != model.ToolOK {
		t.Fatalf("工具事件状态应为 ok，得到 %s", sdb.toolEvents[0].Status)
	}
	// 消息：assistant(tool_calls) + tool 结果 + assistant(最终)
	if len(sdb.messages) != 3 {
		t.Fatalf("应落 3 条消息，得到 %d", len(sdb.messages))
	}
	if sdb.messages[0].Role != model.RoleAssistant || len(sdb.messages[0].ToolCallsJSON) == 0 {
		t.Fatal("第 1 条应为带 tool_calls 的 assistant")
	}
	if sdb.messages[1].Role != model.RoleTool {
		t.Fatalf("第 2 条应为 tool，得到 %s", sdb.messages[1].Role)
	}
	if sdb.messages[2].Content != "工具已执行" {
		t.Fatalf("第 3 条内容错误: %q", sdb.messages[2].Content)
	}
}

// 执行面归属落库（C-5）：端侧结果必须 voice executor=device / device_id / source_trust=untrusted。
func TestEngineToolEventExecutorPersisted(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{
			{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "fs_read"}},
			{ToolCall: &llm.ToolCallDelta{Index: 0, ArgumentsDelta: `{"path":"a.txt"}`}},
		},
		{{Delta: "读到了"}},
	}}
	tools := &fakeTools{result: ToolRunResult{
		Status:      model.ToolOK,
		ResultJSON:  `{"content":"端侧内容"}`,
		Executor:    model.ExecutorDevice,
		DeviceID:    "dev-9",
		SourceTrust: model.TrustUntrusted,
	}}
	eng, sdb, _, _ := newTestEngine(t, provider, func(d *Deps) { d.Tools = tools })

	if err := eng.Execute(context.Background(), baseRunRequest()); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if len(sdb.toolEvents) != 1 {
		t.Fatalf("应有 1 条工具事件，得到 %d", len(sdb.toolEvents))
	}
	ev := sdb.toolEvents[0]
	if ev.Executor != model.ExecutorDevice || ev.DeviceID != "dev-9" {
		t.Fatalf("执行面归属未落库: executor=%q device_id=%q", ev.Executor, ev.DeviceID)
	}
	if ev.SourceTrust != model.TrustUntrusted {
		t.Fatalf("端侧结果必须落 untrusted（不变量 4），得到 %q", ev.SourceTrust)
	}
}

// 静默降级落库（C-5）：本地执行 → server + trusted（且不得因降级产生用户可见错误）。
func TestEngineToolEventDegradedDefaults(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "echo"}}},
		{{Delta: "完成"}},
	}}
	// 执行面字段留零值（等价于未接设备域 / 降级本地）
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK, ResultJSON: `{"ok":true}`}}
	eng, sdb, _, _ := newTestEngine(t, provider, func(d *Deps) { d.Tools = tools })

	if err := eng.Execute(context.Background(), baseRunRequest()); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	ev := sdb.toolEvents[0]
	if ev.Executor != model.ExecutorServer || ev.SourceTrust != model.TrustTrusted || ev.DeviceID != "" {
		t.Fatalf("零值应兜底为 server/trusted 且无 device_id，得到 %+v", ev)
	}
}

// 审批超时视为拒绝：工具被拒，原因回灌模型，Run 继续并完成。
func TestEngineApprovalTimeoutDeniesAndContinues(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "write_file"}}},
		{{Delta: "好的，我换个方式"}},
	}}
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK}}
	// 短超时闸门 + confirm 模式 → 必然超时拒绝
	eng, sdb, _, _ := newTestEngine(t, provider, func(d *Deps) {
		d.Tools = tools
		d.Gate = approval.NewGate(50 * time.Millisecond)
	})

	req := baseRunRequest()
	req.AccessMode = approval.ModeConfirm
	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("拒绝后 Run 应继续完成: %v", err)
	}
	if tools.calls != 0 {
		t.Fatalf("被拒工具不应执行，实际执行 %d 次", tools.calls)
	}
	if len(sdb.toolEvents) != 1 || sdb.toolEvents[0].Status != model.ToolDenied {
		t.Fatalf("工具事件应为 denied，得到 %+v", sdb.toolEvents)
	}
	if sdb.toolEvents[0].Error == "" {
		t.Fatal("拒绝原因必须落库（回传模型）")
	}
	if sdb.run.Status != model.RunCompleted {
		t.Fatalf("Run 应完成，得到 %s", sdb.run.Status)
	}
}

// 访问模式切换即时生效：Run 以 confirm 启动，审批时实时读到 full → write 工具自动放行。
func TestEngineAccessModeLiveUpdate(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "write_file"}}},
		{{Delta: "完成"}},
	}}
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK}}
	eng, sdb, _, _ := newTestEngine(t, provider, func(d *Deps) {
		d.Tools = tools
		d.Gate = approval.NewGate(50 * time.Millisecond)
		// 模拟「运行中把会话切到 full」：审批时读到的不再是启动快照
		d.AccessModeProvider = func(ctx context.Context, sessionID string) (approval.AccessMode, error) {
			return approval.ModeFull, nil
		}
	})

	req := baseRunRequest()
	req.AccessMode = approval.ModeConfirm // 启动快照仍是 confirm
	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("Run 应完成: %v", err)
	}
	if tools.calls != 1 {
		t.Fatalf("full 模式下 write 工具应自动放行，实际执行 %d 次", tools.calls)
	}
	if len(sdb.toolEvents) != 1 || sdb.toolEvents[0].Status != model.ToolOK {
		t.Fatalf("工具事件应为 ok，得到 %+v", sdb.toolEvents)
	}
}

// AccessModeProvider 读取失败时回落 Run 启动快照（confirm → 审批仍拦截）。
func TestEngineAccessModeFallbackOnError(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "write_file"}}},
		{{Delta: "好的，我换个方式"}},
	}}
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK}}
	eng, sdb, _, _ := newTestEngine(t, provider, func(d *Deps) {
		d.Tools = tools
		d.Gate = approval.NewGate(50 * time.Millisecond)
		d.AccessModeProvider = func(ctx context.Context, sessionID string) (approval.AccessMode, error) {
			return "", errors.New("db down")
		}
	})

	req := baseRunRequest()
	req.AccessMode = approval.ModeConfirm
	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("Run 应完成: %v", err)
	}
	if tools.calls != 0 {
		t.Fatalf("读库失败应回落快照（confirm 拦截），实际执行 %d 次", tools.calls)
	}
	if len(sdb.toolEvents) != 1 || sdb.toolEvents[0].Status != model.ToolDenied {
		t.Fatalf("工具事件应为 denied，得到 %+v", sdb.toolEvents)
	}
}

// recordingProvider 记录最近一次 ChatStream 请求（断言引导回灌进上下文）。
type recordingProvider struct {
	inner   llm.Provider
	mu      sync.Mutex
	lastReq llm.ChatRequest
}

func (p *recordingProvider) Name() string { return p.inner.Name() }
func (p *recordingProvider) ChatStream(ctx context.Context, req llm.ChatRequest) (llm.Stream, error) {
	p.mu.Lock()
	p.lastReq = req
	p.mu.Unlock()
	return p.inner.ChatStream(ctx, req)
}

// 运行中引导（§4.2）：引擎在轮次边界消费 GuidanceCh，把引导消息回灌进下一轮上下文
// （消息由 service 层落库，引擎不重复落库）。
func TestEngineGuidanceInjectedAtTurnBoundary(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "run_command", ArgumentsDelta: `{"cmd":"go","args":["test","./..."]}`}}},
		{{Delta: "收到引导"}},
	}}
	rec := &recordingProvider{inner: provider}
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK}}
	eng, _, _, _ := newTestEngine(t, rec, func(d *Deps) {
		d.Tools = tools
		d.Catalog = fakeCatalog{}
	})

	guidanceCh := make(chan *model.Message, 4)
	guidanceCh <- &model.Message{ID: "g1", RunID: "run-1", Role: model.RoleUser, Content: "顺便跑一下 go vet", Seq: 3}
	req := baseRunRequest()
	req.GuidanceCh = guidanceCh
	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("Run 应完成: %v", err)
	}

	rec.mu.Lock()
	last := rec.lastReq
	rec.mu.Unlock()
	found := false
	for _, m := range last.Messages {
		if m.Role == llm.RoleUser && m.Content == "顺便跑一下 go vet" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("引导消息应回灌进下一轮上下文，请求消息数=%d", len(last.Messages))
	}
}

// fakeCatalog 契约风险级桩（run_command = command）。
type fakeCatalog struct{}

func (fakeCatalog) Risk(name string) (model.Risk, bool) {
	if name == "run_command" {
		return model.RiskCommand, true
	}
	return model.RiskWrite, true
}

// 高危命令特征检测（RiskOverride）：full 模式下 rm -rf 仍因升级为 danger 被逐次审批拦截。
func TestEngineDangerCommandEscalation(t *testing.T) {
	dangerArgs := `{"cmd":"rm","args":["-rf","/tmp/target"]}`
	provider := &fakeProvider{rounds: [][]llm.Chunk{
		{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_1", Name: "run_command", ArgumentsDelta: dangerArgs}}},
		{{Delta: "换个方式"}},
	}}
	tools := &fakeTools{result: ToolRunResult{Status: model.ToolOK}}
	eng, sdb, _, _ := newTestEngine(t, provider, func(d *Deps) {
		d.Tools = tools
		d.Catalog = fakeCatalog{}
		d.Gate = approval.NewGate(50 * time.Millisecond)
		// 与 tools.Runner.RiskFor 同口径：命中高危特征 → danger
		d.RiskOverride = func(name string, args []byte, base model.Risk) model.Risk {
			if name == "run_command" && strings.Contains(string(args), "-rf") {
				return model.RiskDanger
			}
			return base
		}
	})

	req := baseRunRequest()
	req.AccessMode = approval.ModeFull // full 自动放行 write/command，但 danger 必须逐次审批
	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("Run 应完成: %v", err)
	}
	if tools.calls != 0 {
		t.Fatalf("full 模式下高危命令不应自动放行，实际执行 %d 次", tools.calls)
	}
	if len(sdb.toolEvents) != 1 || sdb.toolEvents[0].Status != model.ToolDenied {
		t.Fatalf("高危命令应被审批拦截，得到 %+v", sdb.toolEvents)
	}
}

func TestEngineRejectsNonPendingRun(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{{{Delta: "x"}}}}
	eng, sdb, _, _ := newTestEngine(t, provider)
	sdb.run.Status = model.RunCompleted

	err := eng.Execute(context.Background(), baseRunRequest())
	if err == nil {
		t.Fatal("已完成的 Run 不应能再次执行")
	}
	if sdb.run.Status != model.RunCompleted {
		t.Fatalf("状态不应被改动，得到 %s", sdb.run.Status)
	}
}

// 超过最大步数 → failed + max_steps_exceeded（防死循环）。
func TestEngineMaxStepsGuard(t *testing.T) {
	// 每轮都返回 tool_call，永不结束
	round := []llm.Chunk{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "c", Name: "loop"}}}
	provider := &fakeProvider{rounds: [][]llm.Chunk{round, round, round, round, round, round}}
	eng, sdb, _, _ := newTestEngine(t, provider, func(d *Deps) {
		d.Tools = &fakeTools{result: ToolRunResult{Status: model.ToolOK, ResultJSON: "{}"}}
		d.MaxSteps = 3
	})

	err := eng.Execute(context.Background(), baseRunRequest())
	if err == nil {
		t.Fatal("应因超过步数而失败")
	}
	if sdb.run.Status != model.RunFailed {
		t.Fatalf("Run 应为 failed，得到 %s", sdb.run.Status)
	}
	if sdb.run.ErrorCode != "max_steps_exceeded" {
		t.Fatalf("error_code 应为 max_steps_exceeded，得到 %q", sdb.run.ErrorCode)
	}
}

func TestNewRejectsMissingDeps(t *testing.T) {
	if _, err := New(Deps{}); err == nil {
		t.Fatal("缺依赖应报错")
	}
}

func TestExecuteRejectsEmptyIDs(t *testing.T) {
	provider := &fakeProvider{rounds: [][]llm.Chunk{{{Delta: "x"}}}}
	eng, _, _, _ := newTestEngine(t, provider)
	if err := eng.Execute(context.Background(), RunRequest{}); err == nil {
		t.Fatal("空 RunID/SessionID 应报错")
	}
}
