// Package subagent 是子代理插件（阶段 F / on_demand 档）。
//
// 经 Host.Spawn 派生嵌套 Run（真实 Run 落库可观测）。两种用法：
//   - 阻塞（默认）：spawn 直接等子 Run 终态，把最终回答作为工具结果回灌；
//   - 非阻塞（wait=false）：立即返回 subagent_id，配合 status/wait/stop/reject
//     组成完整的协作闭环（并行派发 + 验收打回）。
//
// 禁递归由内核 ChildRunner 强制（深度上限 2），插件侧不重复实现——绕不过内核。
package subagent

import (
	_ "embed"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

//go:embed manifest.json
var manifestJSON []byte

// StorePath 子代理派发记录（驳回返工需要原任务描述，必须落盘可查）。
const StorePath = ".harness/subagents.json"

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
				"spawn_subagent":      spawnSubagent,
				"get_subagent_status": getSubagentStatus,
				"wait_subagents":      waitSubagents,
				"reject_subagent":     rejectSubagent,
				"stop_subagent":       stopSubagent,
			},
		}, nil
	}}
}
