// Package command 是命令执行插件（阶段 F / core 档）。
//
// 风险级 command → 引擎审批闸门裁决后才执行（需求 §10-4「执行命令触发审批，
// 可拒绝并回传原因」）。参数不经 shell 解释（args 数组直传），防注入。
// 实时输出：增量经 Host.Events 以 tool.progress 推送（需求 §10-4）。
package command

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
				"run_command": invokeRunCommand,
			},
		}, nil
	}}
}
