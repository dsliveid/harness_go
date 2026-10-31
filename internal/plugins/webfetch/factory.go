// Package webfetch 是网页抓取插件（阶段 F / deferred 档）。
//
// 安全边界（docs/MCP接入设计.md §10 同源思想）：
//   - 仅 http/https；**私有/环回网段默认拦截**（SSRF 防护，防 localhost 管理端点探测）；
//   - 响应体积上限 1MB、文本上限默认 16k 字符；
//   - 仅接受文本类 Content-Type（HTML 按「去标签」近似提取正文）。
package webfetch

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
				"web_fetch": invokeFetch,
			},
		}, nil
	}}
}
