// Package image 是图片插件（阶段 F / on_demand 档）。
//
// 一期能力：base64 图片落工作区（魔数校验）与元信息读取（宽高/格式/大小）。
// generate_image / recognize_image 需要「图像模型槽位」（能力路由，基础服务设计
// §199 S3）——当前 provider 面无图像能力，调用即返回诚实错误，不假装可用。
package image

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
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
				"save_image":      invokeSave,
				"read_image_meta": invokeMeta,
			},
		}, nil
	}}
}

// sniffImage 魔数校验（防把任意二进制伪装成图片落盘）。
func sniffImage(data []byte) (mime string, ok bool) {
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "image/png", true
	case len(data) >= 3 && bytes.Equal(data[:3], []byte("\xff\xd8\xff")):
		return "image/jpeg", true
	case len(data) >= 6 && (bytes.Equal(data[:6], []byte("GIF87a")) || bytes.Equal(data[:6], []byte("GIF89a"))):
		return "image/gif", true
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp", true
	}
	return "", false
}

func invokeSave(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Path    string `json:"path"`
		DataB64 string `json:"data_base64"`
		MIME    string `json:"mime"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("save_image: %v", err), nil
	}
	if strings.TrimSpace(args.Path) == "" {
		return pluginkit.ErrorResult("save_image: path 不能为空"), nil
	}
	data, err := base64.StdEncoding.DecodeString(args.DataB64)
	if err != nil {
		return pluginkit.ErrorResult("save_image: base64 解码失败: %v", err), nil
	}
	if len(data) > 20<<20 {
		return pluginkit.ErrorResult("save_image: 图片超过 20MB 上限"), nil
	}
	got, ok := sniffImage(data)
	if !ok {
		return pluginkit.ErrorResult("save_image: 内容不是可识别的图片（PNG/JPEG/GIF/WebP 魔数校验失败）"), nil
	}
	if args.MIME != "" && args.MIME != got {
		return pluginkit.ErrorResult("save_image: 声明类型 %s 与实际魔数 %s 不符", args.MIME, got), nil
	}
	if err := host.Workspace().WriteFile(ctx, args.Path, data, 0o644); err != nil {
		return pluginkit.ErrorResult("save_image: 写入失败: %v", err), nil
	}
	cfg, _, derr := image.DecodeConfig(bytes.NewReader(data))
	if derr != nil {
		return pluginkit.ErrorResult("save_image: 图片解析失败: %v", derr), nil
	}
	return pluginkit.TextResult("已保存 %s（%s，%dx%d，%d 字节）",
		args.Path, got, cfg.Width, cfg.Height, len(data)), nil
}

func invokeMeta(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("read_image_meta: %v", err), nil
	}
	data, err := host.Workspace().ReadFile(ctx, args.Path)
	if err != nil {
		return pluginkit.ErrorResult("read_image_meta: 读取失败: %v", err), nil
	}
	mime, ok := sniffImage(data)
	if !ok {
		return pluginkit.ErrorResult("read_image_meta: %s 不是可识别的图片", args.Path), nil
	}
	cfg, _, derr := image.DecodeConfig(bytes.NewReader(data))
	if derr != nil {
		return pluginkit.ErrorResult("read_image_meta: 解析失败: %v", derr), nil
	}
	return pluginkit.TextResult("%s：%s %dx%d，%d 字节", args.Path, mime, cfg.Width, cfg.Height, len(data)), nil
}
