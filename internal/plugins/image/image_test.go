package image

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"harness-go/internal/tools/hostimpl"
	"harness-go/pkg/toolapi"
)

// ============ 测试辅助 ============

// pngBytes 生成一张合法的小 PNG（带真实 IHDR，可被 DecodeConfig 解析）。
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{R: 10, G: 20, B: 30, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("构造测试 PNG 失败: %v", err)
	}
	return buf.Bytes()
}

// newHost 返回绑定了临时工作区的 hostimpl（未注入图像能力）。
func newHost(t *testing.T) *hostimpl.Host {
	t.Helper()
	return hostimpl.New(hostimpl.Options{WorkDir: t.TempDir()})
}

// newPlugin 用 host 构造插件实例。
func newPlugin(t *testing.T, host toolapi.Host) toolapi.Plugin {
	t.Helper()
	p, err := Factory().New(host, nil)
	if err != nil {
		t.Fatalf("插件构造失败: %v", err)
	}
	return p
}

func callTool(t *testing.T, p toolapi.Plugin, name string, args map[string]any) *toolapi.Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("参数序列化失败: %v", err)
	}
	tool := findTool(t, p, name)
	res, err := tool.Invoke(context.Background(), &toolapi.Call{Name: name, Args: raw})
	if err != nil {
		t.Fatalf("%s 返回硬错误（应为 Result.IsError）: %v", name, err)
	}
	if res == nil {
		t.Fatalf("%s 返回 nil Result", name)
	}
	return res
}

func findTool(t *testing.T, p toolapi.Plugin, name string) toolapi.Tool {
	t.Helper()
	for _, tl := range p.Tools() {
		if tl.Spec().Name == name {
			return tl
		}
	}
	t.Fatalf("插件未注册工具 %s", name)
	return nil
}

// mockProvider 可控的 ImageProvider 桩（绝不触网）。
type mockProvider struct {
	data     []byte
	mime     string
	model    string
	genErr   error
	text     string
	descErr  error
	lastGen  toolapi.ImageGenRequest
	lastDesc toolapi.ImageUnderstandRequest
}

func (m *mockProvider) Generate(_ context.Context, req toolapi.ImageGenRequest) (toolapi.ImageGenResult, error) {
	m.lastGen = req
	if m.genErr != nil {
		return toolapi.ImageGenResult{}, m.genErr
	}
	return toolapi.ImageGenResult{Data: m.data, MIME: m.mime, Model: m.model}, nil
}

func (m *mockProvider) Describe(_ context.Context, req toolapi.ImageUnderstandRequest) (string, error) {
	m.lastDesc = req
	if m.descErr != nil {
		return "", m.descErr
	}
	return m.text, nil
}

// noImageHost 只暴露 Host 接口方法（不提升 Images），用于验证「宿主未实现 ImageHost」分支。
type noImageHost struct{ toolapi.Host }

// ============ 1. manifest 与注册工具一致性 ============

func TestManifestToolsMatchRegistered(t *testing.T) {
	p := newPlugin(t, newHost(t))

	var manifestNames []string
	for _, spec := range Manifest().Tools {
		manifestNames = append(manifestNames, spec.Name)
	}
	var registered []string
	for _, tl := range p.Tools() {
		registered = append(registered, tl.Spec().Name)
	}
	sort.Strings(manifestNames)
	sort.Strings(registered)
	if strings.Join(manifestNames, ",") != strings.Join(registered, ",") {
		t.Fatalf("manifest 工具名与注册工具不一致:\nmanifest=%v\nregistered=%v", manifestNames, registered)
	}

	for _, want := range []string{"save_image", "read_image_meta", "generate_image", "recognize_image"} {
		found := false
		for _, n := range registered {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("缺少工具 %s", want)
		}
	}
}

func TestManifestTierAndRisk(t *testing.T) {
	want := map[string]struct {
		tier toolapi.Tier
		risk toolapi.Risk
	}{
		"save_image":      {toolapi.TierOnDemand, toolapi.RiskWrite},
		"read_image_meta": {toolapi.TierOnDemand, toolapi.RiskReadonly},
		"generate_image":  {toolapi.TierOnDemand, toolapi.RiskWrite},
		"recognize_image": {toolapi.TierOnDemand, toolapi.RiskReadonly},
	}
	for _, spec := range Manifest().Tools {
		w, ok := want[spec.Name]
		if !ok {
			t.Fatalf("manifest 出现未预期工具 %s", spec.Name)
		}
		if spec.Tier != w.tier || spec.Risk != w.risk {
			t.Fatalf("%s tier/risk 不符：got(%s,%s) want(%s,%s)", spec.Name, spec.Tier, spec.Risk, w.tier, w.risk)
		}
	}
}

// ============ 2. save_image / read_image_meta 回归 ============

func TestSaveImageValid(t *testing.T) {
	host := newHost(t)
	p := newPlugin(t, host)

	data := pngBytes(t, 4, 3)
	res := callTool(t, p, "save_image", map[string]any{
		"path":        "out/a.png",
		"data_base64": base64.StdEncoding.EncodeToString(data),
		"mime":        "image/png",
	})
	if res.IsError {
		t.Fatalf("save_image 不应失败: %s", res.Content)
	}
	written, err := host.Workspace().ReadFile(context.Background(), "out/a.png")
	if err != nil {
		t.Fatalf("读取落盘文件失败: %v", err)
	}
	if !bytes.Equal(written, data) {
		t.Fatal("落盘内容与输入不一致")
	}
	if !strings.Contains(res.Content, "4x3") {
		t.Fatalf("结果未包含尺寸信息: %s", res.Content)
	}
}

func TestSaveImageRejectsNonImage(t *testing.T) {
	host := newHost(t)
	p := newPlugin(t, host)

	res := callTool(t, p, "save_image", map[string]any{
		"path":        "bad.png",
		"data_base64": base64.StdEncoding.EncodeToString([]byte("not an image at all")),
		"mime":        "image/png",
	})
	if !res.IsError {
		t.Fatal("非图片数据应被魔数校验拒绝")
	}
	if !strings.Contains(res.Content, "魔数校验失败") {
		t.Fatalf("错误信息未说明魔数校验: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(host.Workspace().Root(), "bad.png")); !os.IsNotExist(err) {
		t.Fatal("校验失败时不应落盘")
	}
}

func TestReadImageMeta(t *testing.T) {
	host := newHost(t)
	p := newPlugin(t, host)
	if err := host.Workspace().WriteFile(context.Background(), "m.png", pngBytes(t, 7, 5), 0o644); err != nil {
		t.Fatalf("预置图片失败: %v", err)
	}
	res := callTool(t, p, "read_image_meta", map[string]any{"path": "m.png"})
	if res.IsError {
		t.Fatalf("read_image_meta 不应失败: %s", res.Content)
	}
	if !strings.Contains(res.Content, "7x5") || !strings.Contains(res.Content, "image/png") {
		t.Fatalf("元信息不符: %s", res.Content)
	}
}

// ============ 3. 未注入 provider 时的诚实错误 ============

func TestGenerateImageNoProvider(t *testing.T) {
	p := newPlugin(t, newHost(t))
	res := callTool(t, p, "generate_image", map[string]any{"prompt": "一只猫", "path": "cat.png"})
	if !res.IsError {
		t.Fatal("未注入 provider 时必须返回错误结果")
	}
	if !strings.Contains(res.Content, "图像能力未启用") {
		t.Fatalf("错误信息未说明能力未启用: %s", res.Content)
	}
}

func TestRecognizeImageNoProvider(t *testing.T) {
	p := newPlugin(t, newHost(t))
	res := callTool(t, p, "recognize_image", map[string]any{"path": "any.png"})
	if !res.IsError || !strings.Contains(res.Content, "图像能力未启用") {
		t.Fatalf("未注入 provider 时必须返回「图像能力未启用」: %+v", res)
	}
}

// 宿主根本不实现 toolapi.ImageHost（类型断言失败）时也必须诚实报错，不能 panic。
func TestImageToolsHostWithoutImageHost(t *testing.T) {
	host := noImageHost{Host: newHost(t)}
	p := newPlugin(t, host)

	gen := callTool(t, p, "generate_image", map[string]any{"prompt": "x", "path": "x.png"})
	if !gen.IsError || !strings.Contains(gen.Content, "图像能力未启用") {
		t.Fatalf("非 ImageHost 宿主的 generate_image 错误不符: %+v", gen)
	}
	rec := callTool(t, p, "recognize_image", map[string]any{"path": "x.png"})
	if !rec.IsError || !strings.Contains(rec.Content, "图像能力未启用") {
		t.Fatalf("非 ImageHost 宿主的 recognize_image 错误不符: %+v", rec)
	}
}

// ============ 4. 注入 mock provider：generate ============

func TestGenerateImageWithMock(t *testing.T) {
	host := newHost(t)
	mock := &mockProvider{data: pngBytes(t, 6, 6), mime: "image/png", model: "mock-image-1"}
	host.SetImages(mock)
	p := newPlugin(t, host)

	res := callTool(t, p, "generate_image", map[string]any{
		"prompt": "  一张日落照片  ",
		"path":   "gen/sunset.png",
		"size":   "1024x1024",
	})
	if res.IsError {
		t.Fatalf("generate_image 不应失败: %s", res.Content)
	}
	if mock.lastGen.Prompt != "一张日落照片" {
		t.Fatalf("prompt 未 trim 或未传递: %q", mock.lastGen.Prompt)
	}
	if mock.lastGen.Size != "1024x1024" {
		t.Fatalf("size 未传递: %q", mock.lastGen.Size)
	}
	if mock.lastGen.N != 1 {
		t.Fatalf("n 应为 1，实际 %d", mock.lastGen.N)
	}
	written, err := host.Workspace().ReadFile(context.Background(), "gen/sunset.png")
	if err != nil {
		t.Fatalf("未落盘: %v", err)
	}
	if !bytes.Equal(written, mock.data) {
		t.Fatal("落盘内容与 provider 返回不一致")
	}
	if !strings.Contains(res.Content, "6x6") || !strings.Contains(res.Content, "mock-image-1") {
		t.Fatalf("结果信息不足: %s", res.Content)
	}
}

func TestGenerateImageRejectsNonImageFromProvider(t *testing.T) {
	host := newHost(t)
	host.SetImages(&mockProvider{data: []byte("MZ\x90 fake binary"), mime: "image/png"})
	p := newPlugin(t, host)

	res := callTool(t, p, "generate_image", map[string]any{"prompt": "x", "path": "evil.png"})
	if !res.IsError {
		t.Fatal("provider 返回非图片数据时必须拒绝")
	}
	if !strings.Contains(res.Content, "魔数校验失败") {
		t.Fatalf("错误信息未说明魔数校验: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(host.Workspace().Root(), "evil.png")); !os.IsNotExist(err) {
		t.Fatal("魔数校验失败时不得落盘")
	}
}

func TestGenerateImageProviderError(t *testing.T) {
	host := newHost(t)
	host.SetImages(&mockProvider{genErr: context.DeadlineExceeded})
	p := newPlugin(t, host)
	res := callTool(t, p, "generate_image", map[string]any{"prompt": "x", "path": "x.png"})
	if !res.IsError || !strings.Contains(res.Content, "生成失败") {
		t.Fatalf("provider 报错时必须返回生成失败: %+v", res)
	}
}

func TestGenerateImageEmptyDataFromProvider(t *testing.T) {
	host := newHost(t)
	host.SetImages(&mockProvider{data: nil})
	p := newPlugin(t, host)
	res := callTool(t, p, "generate_image", map[string]any{"prompt": "x", "path": "x.png"})
	if !res.IsError || !strings.Contains(res.Content, "空图像数据") {
		t.Fatalf("空数据必须显式报错: %+v", res)
	}
}

// ============ 5. 注入 mock provider：recognize ============

func TestRecognizeImageWithMock(t *testing.T) {
	host := newHost(t)
	img := pngBytes(t, 3, 3)
	if err := host.Workspace().WriteFile(context.Background(), "pic.png", img, 0o644); err != nil {
		t.Fatalf("预置图片失败: %v", err)
	}
	mock := &mockProvider{text: "图中是一张纯色测试图"}
	host.SetImages(mock)
	p := newPlugin(t, host)

	res := callTool(t, p, "recognize_image", map[string]any{"path": "pic.png"})
	if res.IsError {
		t.Fatalf("recognize_image 不应失败: %s", res.Content)
	}
	if !strings.Contains(res.Content, "图中是一张纯色测试图") {
		t.Fatalf("结果未包含描述文本: %s", res.Content)
	}
	if !bytes.Equal(mock.lastDesc.Image, img) {
		t.Fatal("传给 provider 的图片字节不一致")
	}
	if mock.lastDesc.MIME != "image/png" {
		t.Fatalf("MIME 判定错误: %s", mock.lastDesc.MIME)
	}
	if mock.lastDesc.Question != "请描述这张图片的内容" {
		t.Fatalf("默认 question 不符: %q", mock.lastDesc.Question)
	}

	// 自定义 question 透传
	res = callTool(t, p, "recognize_image", map[string]any{"path": "pic.png", "question": "图里有几只猫？"})
	if res.IsError || mock.lastDesc.Question != "图里有几只猫？" {
		t.Fatalf("自定义 question 未透传: %q / %s", mock.lastDesc.Question, res.Content)
	}
}

func TestRecognizeImageNonImageFile(t *testing.T) {
	host := newHost(t)
	if err := host.Workspace().WriteFile(context.Background(), "t.txt", []byte("plain text"), 0o644); err != nil {
		t.Fatalf("预置文本失败: %v", err)
	}
	host.SetImages(&mockProvider{text: "不该被调用"})
	p := newPlugin(t, host)
	res := callTool(t, p, "recognize_image", map[string]any{"path": "t.txt"})
	if !res.IsError || !strings.Contains(res.Content, "魔数校验失败") {
		t.Fatalf("非图片必须拒绝: %+v", res)
	}
}

func TestRecognizeImageEmptyDescription(t *testing.T) {
	host := newHost(t)
	if err := host.Workspace().WriteFile(context.Background(), "pic.png", pngBytes(t, 2, 2), 0o644); err != nil {
		t.Fatalf("预置图片失败: %v", err)
	}
	host.SetImages(&mockProvider{text: "   "})
	p := newPlugin(t, host)
	res := callTool(t, p, "recognize_image", map[string]any{"path": "pic.png"})
	if !res.IsError || !strings.Contains(res.Content, "空描述") {
		t.Fatalf("空描述必须显式报错: %+v", res)
	}
}

// ============ 6. 参数校验 ============

func TestGenerateImageArgValidation(t *testing.T) {
	host := newHost(t)
	host.SetImages(&mockProvider{data: pngBytes(t, 2, 2), mime: "image/png"})
	p := newPlugin(t, host)

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"空 prompt", map[string]any{"prompt": "   ", "path": "a.png"}, "prompt 不能为空"},
		{"缺 prompt", map[string]any{"path": "a.png"}, "prompt 不能为空"},
		{"空 path", map[string]any{"prompt": "猫", "path": "  "}, "path 不能为空"},
		{"缺 path", map[string]any{"prompt": "猫"}, "path 不能为空"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := callTool(t, p, "generate_image", c.args)
			if !res.IsError || !strings.Contains(res.Content, c.want) {
				t.Fatalf("期望包含 %q，实际 %+v", c.want, res)
			}
		})
	}
}

func TestRecognizeImageArgValidation(t *testing.T) {
	host := newHost(t)
	host.SetImages(&mockProvider{text: "ok"})
	p := newPlugin(t, host)
	for _, args := range []map[string]any{{}, {"path": "   "}} {
		res := callTool(t, p, "recognize_image", args)
		if !res.IsError || !strings.Contains(res.Content, "path 不能为空") {
			t.Fatalf("空 path 必须报错: %+v", res)
		}
	}
}

// 路径越界（逃逸工作区）必须被 workspace 沙箱拦截，且不得写盘。
func TestGenerateImagePathEscape(t *testing.T) {
	host := newHost(t)
	host.SetImages(&mockProvider{data: pngBytes(t, 2, 2), mime: "image/png"})
	p := newPlugin(t, host)

	for _, bad := range []string{"../escape.png", "a/../../escape.png", "/absolute.png", `..\escape.png`, "C:/escape.png"} {
		res := callTool(t, p, "generate_image", map[string]any{"prompt": "猫", "path": bad})
		if !res.IsError {
			t.Fatalf("越界路径 %q 必须被拒绝，实际 %+v", bad, res)
		}
		if !strings.Contains(res.Content, "相对路径") {
			t.Fatalf("越界路径错误信息不明确: %s", res.Content)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(host.Workspace().Root()), "escape.png")); !os.IsNotExist(err) {
		t.Fatal("越界路径不应产生文件")
	}
}

func TestRecognizeImagePathEscape(t *testing.T) {
	host := newHost(t)
	host.SetImages(&mockProvider{text: "ok"})
	p := newPlugin(t, host)
	res := callTool(t, p, "recognize_image", map[string]any{"path": "../secret.png"})
	if !res.IsError || !strings.Contains(res.Content, "相对路径") {
		t.Fatalf("越界读取必须失败: %+v", res)
	}
}

// validWorkspaceRel：Windows 下 filepath.IsAbs("/a.png") 为 false，必须显式拦截。
func TestValidWorkspaceRel(t *testing.T) {
	ok := []string{"a.png", "gen/sunset.png", "gen/sub/deep.png", "./a.png"}
	bad := []string{"", "  ", "/abs.png", `\abs.png`, "~/.x/y.png", "..", "../a.png",
		"a/../../b.png", `..\a.png`, "C:/a.png", `C:\a.png`, "C:a.png"}

	for _, p := range ok {
		if !validWorkspaceRel(p) {
			t.Fatalf("相对路径 %q 应通过校验", p)
		}
	}
	for _, p := range bad {
		if validWorkspaceRel(p) {
			t.Fatalf("越界/绝对路径 %q 应被拒绝", p)
		}
	}
}

// ============ 7. sniffImage 单测（回归既有魔数校验） ============

func TestSniffImage(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
		ok   bool
	}{
		{"png", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 8)...), "image/png", true},
		{"jpeg", []byte("\xff\xd8\xff\xe0rest"), "image/jpeg", true},
		{"gif87", []byte("GIF87a...."), "image/gif", true},
		{"gif89", []byte("GIF89a...."), "image/gif", true},
		{"webp", append(append([]byte("RIFF"), []byte{0, 0, 0, 0}...), []byte("WEBPVP8 ")...), "image/webp", true},
		{"not-image", []byte("hello world"), "", false},
		{"empty", nil, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := sniffImage(c.data)
			if ok != c.ok || got != c.want {
				t.Fatalf("sniffImage(%s) = (%q,%v)，期望 (%q,%v)", c.name, got, ok, c.want, c.ok)
			}
		})
	}
}
