package main

// 阶段 D 出口门禁（docs/开发实施计划.md §5「D 阶段出口门禁」的可机器断言部分）：
//
//   - 固定开销 ≤8k：真实装配（默认 systemBase + 真实插件注册表 + 真实预算闸门）下，
//     零历史/零输入的 prompt 总额不得超过 8000 token —— 该断言随阶段 F 工具增多
//     持续生效，是「工具市场一上线就自我毒化」的定量防线；
//   - /api/v1/tools/injection 返回的预算明细与组装层口径一致（端点是
//     loader.Injection 的直通视图，服务层断言即端点断言）。
//
// cache_hit>60% 与工具选择准确率需要真实 LLM 评测集，属接入真实工具（阶段 F）
// 后的收口项：当前内置工具仅 echo，跑评测无意义；本文件只固定可确定断言的部分。

import (
	"context"
	"io"
	"testing"

	"harness-go/internal/config"
	"harness-go/internal/observ"
	"harness-go/internal/prompt"
	"harness-go/internal/service"
	"harness-go/internal/tokenizer"
	"harness-go/internal/tools"
	"harness-go/internal/tools/hostimpl"
	"harness-go/internal/tools/loader"
	"harness-go/internal/tools/registry"
	"harness-go/internal/tools/router"
	"harness-go/pkg/toolapi"
)

// dExitGateAssembly 按真实装配链构造：注册表 → 内置插件 → Runner → 闸门包装的 Builder。
type dExitGateAssembly struct {
	runner  *tools.Runner
	gate    *service.BudgetGate
	builder *prompt.Builder
}

func newDExitGateAssembly(t *testing.T) *dExitGateAssembly {
	t.Helper()
	cfg := config.Default()
	logger := observ.NewLogger("error", "text", io.Discard)

	reg := registry.New(registry.Options{
		CoreSoftLimit:     cfg.Tools.CoreSoftLimit,
		DeferredSoftLimit: cfg.Tools.DeferredSoftLimit,
		Logger:            logger,
	})
	host := hostimpl.New(hostimpl.Options{WorkDir: t.TempDir()})
	ok, fail := registerBuiltins(context.Background(), reg, host, nil, logger)
	if fail != 0 || ok == 0 {
		t.Fatalf("内置插件装配异常: ok=%d fail=%d", ok, fail)
	}
	runner := tools.NewRunner(reg, router.New(router.Options{
		Executor:     toolapi.ExecutorServer,
		Capabilities: map[string]bool{toolapi.CapWorkspaceRead: true},
	}), loader.Limits{CoreSoft: cfg.Tools.CoreSoftLimit, DeferredSoft: cfg.Tools.DeferredSoftLimit})

	builder := prompt.NewBuilder(tokenizer.NewRegistry().For(""), prompt.Limits{
		MaxTotal:         cfg.Budget.MaxTotal,
		MaxHistory:       0, // max_total=0 时不预裁历史
		MaxDeferredIndex: cfg.Tools.DeferredSoftLimit,
		MaxSOP:           6,
	})
	return &dExitGateAssembly{
		runner:  runner,
		gate:    service.NewBudgetGate(builder, cfg.Budget.MaxTotal, cfg.Tools.DynamicMinTopK),
		builder: builder,
	}
}

// TestDExitGateFixedOverheadWithin8k 固定开销出口门禁：默认装配 ≤8000 token。
func TestDExitGateFixedOverheadWithin8k(t *testing.T) {
	a := newDExitGateAssembly(t)
	res := a.gate.Build(prompt.Input{
		SystemBase: defaultSystemBase,
		Tools:      a.runner.PromptRefs(),
	})
	if res.OverBudget {
		t.Fatalf("默认装配不应超预算")
	}
	if len(res.Degrade) != 0 {
		t.Fatalf("默认装配（max_total=0）不得触发降级: %v", res.Degrade)
	}
	if res.Breakdown.Total > 8000 {
		t.Fatalf("固定开销 %d token 超出阶段 D 出口门禁（≤8k）: %+v",
			res.Breakdown.Total, res.Breakdown)
	}
}

// TestDExitGateInjectionBudgetConsistent 注入预算明细自洽，且与组装层同量级在门禁内。
func TestDExitGateInjectionBudgetConsistent(t *testing.T) {
	a := newDExitGateAssembly(t)
	inj := a.runner.Injection()
	if inj.CostTotal != inj.CostCore+inj.CostIndex {
		t.Fatalf("注入成本应为 core+index 之和: %d vs %d+%d",
			inj.CostTotal, inj.CostCore, inj.CostIndex)
	}
	if inj.CostTotal > 8000 {
		t.Fatalf("工具注入成本 %d 超出阶段 D 出口门禁（≤8k）", inj.CostTotal)
	}
	// 端点直通视图的字段必须齐备（/api/v1/tools/injection 的 payload 形状）
	if len(inj.Core) == 0 {
		t.Fatalf("core 注入清单不得为空（echo 为 core 档）")
	}
	if inj.Limits.CoreSoft == 0 {
		t.Fatalf("软上限字段缺失: %+v", inj.Limits)
	}
}
