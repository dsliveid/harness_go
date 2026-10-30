package main

// 内置插件唯一装配点（docs/插件体系设计.md D12）。
//
// 约定：
//   - 插件实现在 internal/plugins/<name>/（manifest.json + factory.go + 实现），
//     只依赖 pkg/toolapi，**禁止** import internal/{store,llm,agent,api,service}（depguard 强制）；
//   - 新增内置插件只能改本文件的 builtinFactories 列表（改动收敛到一行）；
//   - 零改动的扩展路径是外部通道（MCP / HTTP 插件 / 本地工具包），不是改内核。

import (
	"context"
	"log/slog"

	"harness-go/internal/plugins/echo"
	"harness-go/internal/tools/registry"
	"harness-go/pkg/toolapi"
)

// builtinFactories 内置插件清单（顺序即注册顺序）。
var builtinFactories = []toolapi.Factory{
	echo.Factory(),
}

// registerBuiltins 装配全部内置插件，返回 (成功数, 失败数)。
//
// 单个插件失败**不阻断启动**（仅告警）：工具能力属于可降级项，
// 内核对话链路必须保持可用（与「软上限只告警」同一原则）。
func registerBuiltins(ctx context.Context, reg *registry.Registry, host toolapi.Host, cfg toolapi.Config, logger *slog.Logger) (int, int) {
	var okCount, failCount int
	for _, f := range builtinFactories {
		if err := reg.RegisterFactory(ctx, f, host, cfg); err != nil {
			failCount++
			if logger != nil {
				logger.Error("内置插件装配失败（已跳过）", "plugin", f.Manifest().ID, "err", err)
			}
			continue
		}
		okCount++
	}
	return okCount, failCount
}
