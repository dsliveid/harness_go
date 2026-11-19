package main

// 上下文压缩的装配（main 包内，因为它需要同时看到 config / store / service）。
//
// # 为什么放在 main 而不是 service
//
// 这是一个纯粹的**装配决策**：用哪个存储、哪个模型、开关是否打开。
// 决策依赖配置，而配置只在 main 层可见。service 层应当"被装配"而不是
// "自己读配置"——否则它会在测试里被迫准备一份完整 config。
//
// # 为什么默认开启且必须给真实窗口
//
// 历史事故（会话 9ee34e00，111 万 token）的制度性根因是：
// 五级降级链的开关 budget.max_total 默认 0（不限）→ 整条链从不触发；
// 而第五级「强制压缩」又只有信号没有消费者。
//
// 即「机制存在，但常年休眠」。因此这里的默认值必须是**真实生效的值**，
// 而不是再一个 0/空值。

import (
	"context"

	"harness-go/internal/agent"
	"harness-go/internal/compact"
	"harness-go/internal/config"
	"harness-go/internal/service"
	"harness-go/internal/store"
)

// buildCompactor 按配置装配上下文压缩器。
//
// 返回 nil 表示不启用压缩（引擎会自动退回改造前的行为）。
func buildCompactor(
	cfg config.Config,
	sessions *store.SessionStore,
	resolver agent.ModelResolver,
) agent.Compactor {
	if !cfg.Compaction.Enabled {
		return nil
	}
	// 窗口未知时装配了也不会生效（compact.Evaluate 会直接跳过）。
	// 这里显式返回 nil，让"未生效"这件事在装配期就可见，而不是运行期静默。
	if cfg.Compaction.MaxContextTokens <= 0 {
		return nil
	}
	return &service.CompactionAdapter{
		Sessions: sessions,
		// 摘要器按 modelRef 解析：不同会话可能用不同模型与密钥。
		// 解析失败时返回 nil，适配器会安全退回"不压缩"。
		SummarizerFor: func(ctx context.Context, modelRef string) compact.Summarizer {
			if modelRef == "" {
				return nil
			}
			provider, modelName, err := resolver.Resolve(ctx, modelRef)
			if err != nil || provider == nil {
				return nil
			}
			return &service.LLMSummarizer{Provider: provider, Model: modelName}
		},
	}
}
