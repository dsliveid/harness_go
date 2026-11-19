package agent

import (
	"context"
	"testing"
	"time"

	"harness-go/internal/llm"
	"harness-go/internal/model"
	"harness-go/internal/prompt"
)

// countingCompactor 水位恒为真的压缩桩，用于验证"压缩是否真的被触发"。
type countingCompactor struct {
	needsCalls int
	compact    int
}

func (c *countingCompactor) NeedsCompaction(_, limit int) bool {
	c.needsCalls++
	if limit <= 0 {
		return false
	}
	return true
}

func (c *countingCompactor) Compact(_ context.Context, _ CompactRequest) (CompactResult, error) {
	c.compact++
	return CompactResult{Compacted: true, StartSeq: 1, EndSeq: 2, ReplacedMessages: 2, SavedTokens: 100}, nil
}

func (c *countingCompactor) History(context.Context, string, []model.Message) ([]llm.Message, error) {
	return []llm.Message{{Role: llm.RoleUser, Content: "摘要"}}, nil
}

// warnBuilder 让组装结果带上指定 token 数（模拟"历史很大"）。
type warnBuilder struct {
	inner ContextBuilder
	total int
}

func (b *warnBuilder) Build(in prompt.Input) prompt.BuildResult {
	res := b.inner.Build(in)
	res.Breakdown.Total = b.total
	return res
}

// TestCompactionFiresWhenMaxTotalZero 回归：budget.max_total=0 时压缩必须仍然生效。
//
// # 这条用例锁定的是一个我亲手引入又亲手修掉的缺陷
//
// 上一版把压缩挂在 res.OverBudget 之下，而：
//
//	prompt/builder.go: overBudget := MaxTotal > 0 && Total > MaxTotal
//	config 默认值:     budget.max_total: 0
//
// 于是 OverBudget 恒为 false → 压缩在**默认配置下永不执行**。
// 讽刺的是，被修的原始缺陷正是"机制存在但常年休眠"，我在同一处换形态重犯。
//
// 根因是轴混用：压缩治的是「窗口容量」，却继承了「预算成本」的开关。
// 现在压缩按窗口水位独立判断，与 max_total 彻底解耦。
func TestCompactionFiresWhenMaxTotalZero(t *testing.T) {
	comp := &countingCompactor{}
	// MaxTotal=0（默认配置）：预算法永远不报超限。
	base := prompt.NewBuilder(nil, prompt.Limits{MaxTotal: 0})
	builder := &warnBuilder{inner: base, total: 60000} // 60k，超过 65536 的 80%

	eng, _, _, _ := newTestEngine(t, &fakeProvider{rounds: [][]llm.Chunk{
		{{Delta: "好的"}},
	}}, func(d *Deps) {
		d.Builder = builder
		d.Compactor = comp
		d.MaxContextTokens = 65536
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = eng.Execute(ctx, baseRunRequest())

	if comp.needsCalls == 0 {
		t.Fatal("引擎必须询问 NeedsCompaction（否则压缩仍处于休眠状态）")
	}
	if comp.compact == 0 {
		t.Fatal("max_total=0 时压缩必须仍然触发（这正是本次修复的目标）")
	}
}

// TestCompactionNotFiredBelowWatermark 未达水位不得压缩。
//
// 防止修过头：解耦后不能变成"每轮都压缩"。
func TestCompactionNotFiredBelowWatermark(t *testing.T) {
	comp := &fakeCompactor{needs: false}
	base := prompt.NewBuilder(nil, prompt.Limits{MaxTotal: 0})
	builder := &warnBuilder{inner: base, total: 1000} // 远低于 80%

	eng, _, _, _ := newTestEngine(t, &fakeProvider{rounds: [][]llm.Chunk{
		{{Delta: "好的"}},
	}}, func(d *Deps) {
		d.Builder = builder
		d.Compactor = comp
		d.MaxContextTokens = 65536
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = eng.Execute(ctx, baseRunRequest())

	if comp.calls != 0 {
		t.Fatalf("未达水位不得压缩，实际压缩 %d 次", comp.calls)
	}
}

// TestCompactionSkippedWhenWindowUnknown 窗口未知（limit<=0）时不得压缩。
//
// 基于猜测的窗口做破坏性折叠，比让 provider 明确报错更糟。
func TestCompactionSkippedWhenWindowUnknown(t *testing.T) {
	comp := &countingCompactor{}
	base := prompt.NewBuilder(nil, prompt.Limits{MaxTotal: 0})
	builder := &warnBuilder{inner: base, total: 999999}

	eng, _, _, _ := newTestEngine(t, &fakeProvider{rounds: [][]llm.Chunk{
		{{Delta: "好的"}},
	}}, func(d *Deps) {
		d.Builder = builder
		d.Compactor = comp
		d.MaxContextTokens = 0 // 窗口未知
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = eng.Execute(ctx, baseRunRequest())

	if comp.compact != 0 {
		t.Fatalf("窗口未知时不得压缩，实际压缩 %d 次", comp.compact)
	}
}

// TestCompactionLimitedPerRun 单 Run 压缩次数必须有上限。
//
// 若无限流，长任务会变成"每步一次摘要调用"，花在摘要上的钱比正文还多。
func TestCompactionLimitedPerRun(t *testing.T) {
	if maxCompactionsPerRun <= 0 {
		t.Fatal("maxCompactionsPerRun 必须为正数")
	}
	// 用一个多轮工具调用场景，让 loop 跑满 MaxSteps。
	comp := &countingCompactor{}
	base := prompt.NewBuilder(nil, prompt.Limits{MaxTotal: 0})
	builder := &warnBuilder{inner: base, total: 60000}

	rounds := make([][]llm.Chunk, 0, 10)
	for i := 0; i < 10; i++ {
		rounds = append(rounds, []llm.Chunk{
			{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_" + itoaTest(i), Name: "read_file"}},
		})
	}
	eng, _, _, _ := newTestEngine(t, &fakeProvider{rounds: rounds}, func(d *Deps) {
		d.Builder = builder
		d.Compactor = comp
		d.MaxContextTokens = 65536
		d.MaxSteps = 8
		d.Tools = &fakeTools{result: ToolRunResult{Status: model.ToolOK, ResultJSON: `{"content":"x"}`}}
		d.Catalog = fakeCatalog{}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = eng.Execute(ctx, baseRunRequest())

	if comp.compact > maxCompactionsPerRun {
		t.Fatalf("单 Run 压缩次数不得超过 %d，实际 %d", maxCompactionsPerRun, comp.compact)
	}
}

// itoaTest 极简整数转串（避免为测试引入 strconv 依赖干扰可读性）。
func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
