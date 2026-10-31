// Package browser 是无头浏览器插件（阶段 F / on_demand 档）。
//
// 能力探测（插件体系设计 §5.1）：config 指定路径 → PATH 查找 → 常见安装目录；
// 缺失时工具返回**显式不可用**说明（绝不静默假装成功），Health 置 unavailable。
//
// 实现：Chromium DevTools Protocol 最小客户端（--headless --remote-debugging-port，
// coder/websocket 复用 C 阶段依赖），Page.navigate + Runtime.evaluate 取渲染文本。
package browser

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
		return &plugin{Host: host}, nil
	}}
}
