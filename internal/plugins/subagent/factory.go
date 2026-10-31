// Package subagent 是子代理插件（阶段 F / on_demand 档）。
//
// 经 Host.Spawn 派生嵌套 Run（真实 Run 落库可观测），阻塞等待终态并把
// 子 Run 的最终回答作为工具结果回灌。禁递归由内核 ChildRunner 强制
// （深度上限 2），插件侧不重复实现——绕不过内核。
package subagent

import (
	_ "embed"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

//go:embed manifest.json
var manifestJSON []byte

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
				"spawn_subagent": invokeSpawn,
			},
		}, nil
	}}
}
