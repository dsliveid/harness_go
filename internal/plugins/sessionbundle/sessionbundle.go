// Package sessionbundle 是会话导出插件（阶段 G / M4）。
//
// 只做**工具入口**（docs/插件体系设计.md §3.4）：打包与恢复的执行体在内核
// （service.BundleService），经 Host.Bundles() 受控访问。插件拿不到 DB、
// 拿不到 data 目录任意路径——导入路径沙箱在内核侧校验（data 目录下或工作区内）。
package sessionbundle

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

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
				"export_session": invokeExport,
				"import_session": invokeImport,
			},
		}, nil
	}}
}

// invokeExport 导出会话（缺省 = 当前会话）。
func invokeExport(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		SessionID string `json:"session_id"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("export_session: %v", err), nil
	}
	target := strings.TrimSpace(args.SessionID)
	if target == "" {
		target = call.SessionID
	}
	if target == "" {
		return pluginkit.ErrorResult("export_session: session_id 不能为空"), nil
	}
	info, err := host.Bundles().Export(ctx, target)
	if err != nil {
		return pluginkit.ErrorResult("export_session: %v", err), nil
	}
	return pluginkit.DataResult(
		fmt.Sprintf("已导出会话 %s：%s（%.1f KB，%d 条消息）", info.SessionID, info.Path,
			float64(info.Size)/1024, info.Messages),
		map[string]any{"bundle": info},
	), nil
}

// invokeImport 从导出包恢复为新会话。
func invokeImport(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		ZipPath string `json:"zip_path"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("import_session: %v", err), nil
	}
	if strings.TrimSpace(args.ZipPath) == "" {
		return pluginkit.ErrorResult("import_session: zip_path 不能为空（用 export_session 返回的路径）"), nil
	}
	info, err := host.Bundles().Import(ctx, args.ZipPath)
	if err != nil {
		return pluginkit.ErrorResult("import_session: %v", err), nil
	}
	return pluginkit.DataResult(
		fmt.Sprintf("已恢复为新会话 %s（%s，%d 条消息，源包 %s）", info.SessionID, info.Title, info.Messages, info.Path),
		map[string]any{"bundle": info},
	), nil
}
