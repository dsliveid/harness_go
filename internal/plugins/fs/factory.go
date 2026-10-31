// Package fs 是文件操作插件（阶段 F / core 档 + file_outline deferred 档）。
//
// 能力：读取（行号标注）、目录列举、写入与精确编辑（带 unified diff）、
// glob 匹配、正则/字面量全文检索、结构大纲。
// 沙箱：一切路径经 Host.Workspace（相对工作区根，越界即拒绝）；
// 写入风险级 write → 引擎审批闸门裁决（需求 §10-4）。
package fs

import (
	_ "embed"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

//go:embed manifest.json
var manifestJSON []byte

// Manifest 返回插件清单（每次返回解析副本）。
func Manifest() toolapi.Manifest {
	return pluginkit.ManifestFromJSON(manifestJSON)
}

// Factory 返回插件工厂（cmd/harness-go/plugins.go 的唯一装配入口）。
func Factory() toolapi.Factory {
	return toolapi.FactoryFunc{M: Manifest(), NewF: func(host toolapi.Host, _ toolapi.Config) (toolapi.Plugin, error) {
		return &pluginkit.PluginFromSpecs{
			M:    Manifest(),
			Host: host,
			Invokes: map[string]pluginkit.InvokeFunc{
				"read_file":    invokeReadFile,
				"list_dir":     invokeListDir,
				"write_file":   invokeWriteFile,
				"edit_file":    invokeEditFile,
				"glob":         invokeGlob,
				"grep":         invokeGrep,
				"file_outline": invokeFileOutline,
			},
		}, nil
	}}
}
