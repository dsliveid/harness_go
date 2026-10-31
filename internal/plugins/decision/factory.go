// Package decision 是决策记录插件（阶段 F / deferred 档）。
// 落 `.harness/decisions.jsonl`（追加式，最近在前读取）。
package decision

import (
	_ "embed"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

//go:embed manifest.json
var manifestJSON []byte

// StorePath 决策日志相对路径。
const StorePath = ".harness/decisions.jsonl"

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
				"decision_record": invokeRecord,
				"decision_list":   invokeList,
			},
		}, nil
	}}
}
