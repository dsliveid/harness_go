// Package image 是图片插件（阶段 F / on_demand 档）。
//
// 能力（四项）：base64 图片落工作区（魔数校验）、元信息读取（宽高/格式/大小）、
// 图像生成（generate_image）与图像理解/识别（recognize_image）。
//
// 诚实边界：generate_image / recognize_image 依赖「可选图像能力宿主」
// （toolapi.ImageHost，阶段 D 引入）。宿主未实现该接口或未注入 provider 时，
// 两个工具返回「图像能力未启用」的诚实错误 —— 不是 panic，也不是静默成功。
// 生成结果落盘前一律过 sniffImage 魔数校验：上游返回非图片二进制即拒绝写入。
// read_image_meta 仍只解头信息，不解码像素；像素级内容理解统一走 recognize_image。
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
	"path"
	"path/filepath"
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
				"save_image":       invokeSave,
				"read_image_meta":  invokeMeta,
				"generate_image":   invokeGenerate,
				"recognize_image":  invokeRecognize,
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

// imageProvider 通过可选接口 ImageHost 取图像能力；缺失时返回 nil。
//
// 刻意不用「默认桩」：未注入 provider 时必须让调用方拿到诚实错误。
func imageProvider(host toolapi.Host) toolapi.ImageProvider {
	ih, ok := host.(toolapi.ImageHost)
	if !ok {
		return nil
	}
	return ih.Images()
}

// validWorkspaceRel 校验 path 是「工作区内的相对路径」。
//
// ⚠️ Windows 特殊坑：`filepath.IsAbs("/a.png")` 返回 **false**（缺少盘符），
// 于是 "/a.png" 会被 filepath.Clean 归一成 "\a.png" 并静默落在工作区根下，
// 造成「看起来像绝对路径却写进工作区」的语义歧义。因此这里显式判前缀，
// 只放行真正的相对路径；穿越 ".." 一律拒绝（内核沙箱另有一道防线，双保险）。
func validWorkspaceRel(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	if filepath.IsAbs(p) || filepath.VolumeName(p) != "" {
		return false
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || strings.HasPrefix(p, "~") {
		return false
	}
	// 统一下滑为 / 再 Clean，避免 Windows 反斜杠穿越（如 a\..\..\b）漏判。
	clean := path.Clean(strings.ReplaceAll(p, `\`, "/"))
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return false
	}
	return true
}

func invokeGenerate(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Prompt   string `json:"prompt"`
		Path     string `json:"path"`
		Size     string `json:"size"`
		ModelRef string `json:"model_ref"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("generate_image: %v", err), nil
	}
	args.Prompt = strings.TrimSpace(args.Prompt)
	args.Path = strings.TrimSpace(args.Path)
	if args.Prompt == "" {
		return pluginkit.ErrorResult("generate_image: prompt 不能为空"), nil
	}
	if args.Path == "" {
		return pluginkit.ErrorResult("generate_image: path 不能为空"), nil
	}
	if !validWorkspaceRel(args.Path) {
		return pluginkit.ErrorResult("generate_image: path 必须是工作区内的相对路径（不允许绝对路径或 .. 穿越）: %q", args.Path), nil
	}
	prov := imageProvider(host)
	if prov == nil {
		return pluginkit.ErrorResult("generate_image: 图像能力未启用（未配置图像模型 provider）"), nil
	}
	res, err := prov.Generate(ctx, toolapi.ImageGenRequest{
		Prompt:   args.Prompt,
		Size:     args.Size,
		ModelRef: args.ModelRef,
		N:        1,
	})
	if err != nil {
		return pluginkit.ErrorResult("generate_image: 生成失败: %v", err), nil
	}
	if len(res.Data) == 0 {
		return pluginkit.ErrorResult("generate_image: 上游返回空图像数据"), nil
	}
	if len(res.Data) > 20<<20 {
		return pluginkit.ErrorResult("generate_image: 图像超过 20MB 上限"), nil
	}
	mime, ok := sniffImage(res.Data)
	if !ok {
		return pluginkit.ErrorResult("generate_image: 上游返回的数据不是可识别的图片（PNG/JPEG/GIF/WebP 魔数校验失败）"), nil
	}
	// 以实际魔数 mime 为准：上游声明的 res.MIME 只作参考，不作拒写理由。
	if err := host.Workspace().WriteFile(ctx, args.Path, res.Data, 0o644); err != nil {
		return pluginkit.ErrorResult("generate_image: 写入 %s 失败: %v", args.Path, err), nil
	}
	cfg, _, derr := image.DecodeConfig(bytes.NewReader(res.Data))
	if derr != nil {
		return pluginkit.TextResult("已生成 %s（%s，%d 字节；模型 %s）",
			args.Path, mime, len(res.Data), res.Model), nil
	}
	return pluginkit.TextResult("已生成 %s（%s，%dx%d，%d 字节；模型 %s）",
		args.Path, mime, cfg.Width, cfg.Height, len(res.Data), res.Model), nil
}

func invokeRecognize(ctx context.Context, host toolapi.Host, call *toolapi.Call) (*toolapi.Result, error) {
	var args struct {
		Path     string `json:"path"`
		Question string `json:"question"`
	}
	if err := pluginkit.DecodeArgs(call.Args, &args); err != nil {
		return pluginkit.ErrorResult("recognize_image: %v", err), nil
	}
	args.Path = strings.TrimSpace(args.Path)
	if args.Path == "" {
		return pluginkit.ErrorResult("recognize_image: path 不能为空"), nil
	}
	if !validWorkspaceRel(args.Path) {
		return pluginkit.ErrorResult("recognize_image: path 必须是工作区内的相对路径（不允许绝对路径或 .. 穿越）: %q", args.Path), nil
	}
	prov := imageProvider(host)
	if prov == nil {
		return pluginkit.ErrorResult("recognize_image: 图像能力未启用（未配置图像模型 provider）"), nil
	}
	data, err := host.Workspace().ReadFile(ctx, args.Path)
	if err != nil {
		return pluginkit.ErrorResult("recognize_image: 读取 %s 失败: %v", args.Path, err), nil
	}
	if len(data) == 0 {
		return pluginkit.ErrorResult("recognize_image: %s 是空文件", args.Path), nil
	}
	mime, ok := sniffImage(data)
	if !ok {
		return pluginkit.ErrorResult("recognize_image: %s 不是可识别的图片（PNG/JPEG/GIF/WebP 魔数校验失败）", args.Path), nil
	}
	question := strings.TrimSpace(args.Question)
	if question == "" {
		question = "请描述这张图片的内容"
	}
	text, err := prov.Describe(ctx, toolapi.ImageUnderstandRequest{
		Image:    data,
		MIME:     mime,
		Question: question,
	})
	if err != nil {
		return pluginkit.ErrorResult("recognize_image: 识别失败: %v", err), nil
	}
	if strings.TrimSpace(text) == "" {
		return pluginkit.ErrorResult("recognize_image: 上游返回空描述"), nil
	}
	return pluginkit.TextResult("%s（%s，%d 字节）\n%s", args.Path, mime, len(data), text), nil
}
