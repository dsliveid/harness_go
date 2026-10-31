// Package todo 是任务清单插件（阶段 F / core 档）。
//
// 状态落工作区文件 `.harness/todo.json`（唯一真源，跨轮次可见）。
package todo

import (
	_ "embed"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

//go:embed manifest.json
var manifestJSON []byte

// StorePath 清单文件相对路径。
const StorePath = ".harness/todo.json"

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
				"todo_write": invokeWrite,
				"todo_read":  invokeRead,
			},
		}, nil
	}}
}
