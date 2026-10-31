// Package growth 是成长记录插件（阶段 F / deferred 档，轻量三工具）。
// 落 `.harness/memory/growth/growth.jsonl`（追加 + 标记重写），不建状态机。
package growth

import (
	_ "embed"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

//go:embed manifest.json
var manifestJSON []byte

// StorePath 成长日志相对路径。
const StorePath = ".harness/memory/growth/growth.jsonl"

// Manifest 返回插件清单。
func Manifest() toolapi.Manifest {
	return pluginkit.ManifestFromJSON(manifestJSON)
}

// Factory 返回插件工厂。
func Factory() toolapi.Factory {
	return toolapi.FactoryFunc{M: Manifest(), NewF: func(host toolapi.Host, _ toolapi.Config) (toolapi.Plugin, error) {
		return &pluginkit.PluginFromSpecs{
			M:    Manifest(),
			Host: host,
			Invokes: map[string]pluginkit.InvokeFunc{
				"record_growth": invokeRecord,
				"list_growths":  invokeList,
				"apply_growth":  invokeApply,
			},
		}, nil
	}}
}
