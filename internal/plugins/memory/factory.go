// Package memory 是记忆写侧插件（阶段 F / on_demand 档）。
//
// 与 D-3 检索基建的关系：本插件把记忆落为工作区文件（`.harness/memory/<layer>/<file>.md`，
// **文件是唯一真源**）；装配点（cmd/harness-go）在启动时把文件同步进主库
// retrieval_docs（kind = memory_l0/memory_l2/memory_l3），复用 D-3 的召回与打分。
// 插件本身零 store 依赖（D12 铁律）。
package memory

import (
	_ "embed"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

//go:embed manifest.json
var manifestJSON []byte

// MemoryDir 记忆库相对目录（子目录 = 层）。
const MemoryDir = ".harness/memory"

// 层目录名（与 retrieval.Kind 的映射在装配点同步器中完成）。
const (
	LayerCore    = "core"
	LayerNotes   = "notes"
	LayerArchive = "archive"
)

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
				"memory_write": invokeWrite,
				"memory_list":  invokeList,
			},
		}, nil
	}}
}
