package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"harness-go/internal/events"
	"harness-go/internal/llm"
	"harness-go/internal/model"
	"harness-go/internal/prompt"
)

// ============ 预算闸门（D-4）与记忆透传（D-3）的引擎侧契约 ============

// capturingBuilder 捕获 Build 输入并返回可控结果。
type capturingBuilder struct {
	last  prompt.Input
	res   prompt.BuildResult
	calls int
}

func (b *capturingBuilder) Build(in prompt.Input) prompt.BuildResult {
	b.calls++
	b.last = in
	return b.res
}

// TestEngineBudgetExhaustedFailsRun 降级用尽仍超预算 → Run 明确失败 budget_exhausted，
// 绝不把超限内容静默塞给模型（provider 一次都不该被调用）。
func TestEngineBudgetExhaustedFailsRun(t *testing.T) {
	b := &capturingBuilder{res: prompt.BuildResult{OverBudget: true}}
	provider := &fakeProvider{rounds: [][]llm.Chunk{{{Delta: "不应到达"}}}}
	eng, sdb, _, _ := newTestEngine(t, provider, func(d *Deps) { d.Builder = b })

	err := eng.Execute(context.Background(), baseRunRequest())
	if err == nil || !strings.Contains(err.Error(), "budget_exhausted") {
		t.Fatalf("应返回 budget_exhausted 错误，得到: %v", err)
	}
	if sdb.run.Status != model.RunFailed {
		t.Fatalf("Run 应为 failed，得到 %s", sdb.run.Status)
	}
	if sdb.run.ErrorCode != "budget_exhausted" {
		t.Fatalf("错误码应为 budget_exhausted，得到 %q", sdb.run.ErrorCode)
	}
	if provider.idx != 0 {
		t.Fatalf("超预算时不得调用 provider（严禁静默放行），实际调用 %d 次", provider.idx)
	}
}

// TestEngineDegradeEventPublished 每一档降级都必须可观测（事件含档位与明细）。
func TestEngineDegradeEventPublished(t *testing.T) {
	b := &capturingBuilder{res: prompt.BuildResult{
		Degrade: []prompt.DegradeAction{
			{Level: prompt.LevelDropMemory, Name: prompt.LevelName(prompt.LevelDropMemory), Applied: true, Detail: "省略 L2/L3 记忆段"},
		},
	}}
	provider := &fakeProvider{rounds: [][]llm.Chunk{{{Delta: "ok"}, {Usage: &llm.Usage{TotalTokens: 1}}}}}
	eng, sdb, bus, _ := newTestEngine(t, provider, func(d *Deps) { d.Builder = b })

	if err := eng.Execute(context.Background(), baseRunRequest()); err != nil {
		t.Fatalf("降级不是失败：%v", err)
	}
	if sdb.run.Status != model.RunCompleted {
		t.Fatalf("降级后应正常完成，得到 %s", sdb.run.Status)
	}
	// 事件流里应有 scope=budget_gate 的非致命 error 事件（payload 为 JSON）
	sub, err := bus.Subscribe("run-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Unsubscribe(sub)
	found := false
	for ev := range sub.C() {
		if ev.Type != events.TypeError {
			continue
		}
		var m map[string]any
		if json.Unmarshal(ev.Payload, &m) != nil || m["scope"] != "budget_gate" {
			continue
		}
		found = true
		if lvl, ok := m["level"].(float64); !ok || int(lvl) != int(prompt.LevelDropMemory) {
			t.Fatalf("事件应带档位: %+v", m)
		}
	}
	if !found {
		t.Fatalf("降级动作必须发布 budget_gate 事件")
	}
}

// TestEngineMemoryPassthrough 记忆块由 service 召回后经 RunRequest 透传到组装输入。
func TestEngineMemoryPassthrough(t *testing.T) {
	b := &capturingBuilder{res: prompt.BuildResult{}}
	provider := &fakeProvider{rounds: [][]llm.Chunk{{{Delta: "ok"}, {Usage: &llm.Usage{TotalTokens: 1}}}}}
	eng, _, _, _ := newTestEngine(t, provider, func(d *Deps) { d.Builder = b })

	req := baseRunRequest()
	req.MemoryCore = "L0核心约束"
	req.MemoryTail = "## 相关记忆\n- [L2] 结论"
	if err := eng.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if b.last.MemoryCore != "L0核心约束" || b.last.MemoryTail != "## 相关记忆\n- [L2] 结论" {
		t.Fatalf("记忆块必须透传到组装输入: core=%q tail=%q", b.last.MemoryCore, b.last.MemoryTail)
	}
}

// TestEngineMemoryNilDefault 未接记忆源（零值 RunRequest）时行为与 D-3 之前一致。
func TestEngineMemoryNilDefault(t *testing.T) {
	b := &capturingBuilder{res: prompt.BuildResult{}}
	provider := &fakeProvider{rounds: [][]llm.Chunk{{{Delta: "ok"}, {Usage: &llm.Usage{TotalTokens: 1}}}}}
	eng, _, _, _ := newTestEngine(t, provider, func(d *Deps) { d.Builder = b })

	if err := eng.Execute(context.Background(), baseRunRequest()); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if b.last.MemoryCore != "" || b.last.MemoryTail != "" {
		t.Fatalf("未接记忆源时记忆块应为空: %+v", b.last)
	}
}
