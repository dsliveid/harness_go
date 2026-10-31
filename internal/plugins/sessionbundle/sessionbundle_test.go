package sessionbundle

// session-bundle 插件工具单测（plugintest 记录桩；打包/恢复语义由 service 层覆盖）。

import (
	"testing"

	"harness-go/internal/pluginkit/plugintest"
	"harness-go/pkg/toolapi"
)

func invoke(t *testing.T, h *plugintest.Host, name, args string) *toolapi.Result {
	t.Helper()
	p, err := Factory().New(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range p.Tools() {
		if tool.Spec().Name != name {
			continue
		}
		res, iErr := tool.Invoke(t.Context(), &toolapi.Call{
			CallID: name + ":1", Name: name, Args: []byte(args), SessionID: "sess-cur",
		})
		if iErr != nil {
			t.Fatalf("%s invoke: %v", name, iErr)
		}
		return res
	}
	t.Fatalf("插件未声明工具 %s", name)
	return nil
}

// TestExportDefaultsToCurrentSession 缺省导出当前会话（Call.SessionID）。
func TestExportDefaultsToCurrentSession(t *testing.T) {
	h := plugintest.New(t)
	res := invoke(t, h, "export_session", `{}`)
	if res.IsError {
		t.Fatalf("export 失败: %s", res.Content)
	}
	exports := h.BundleStubExports()
	if len(exports) != 1 || exports[0].SessionID != "sess-cur" {
		t.Fatalf("应导出当前会话: %+v", exports)
	}
}

// TestImportRelaysPath import 把路径透传内核（沙箱校验在内核侧）。
func TestImportRelaysPath(t *testing.T) {
	h := plugintest.New(t)
	res := invoke(t, h, "import_session", `{"zip_path":"/data/sessions/s1/bundle/x.zip"}`)
	if res.IsError {
		t.Fatalf("import 失败: %s", res.Content)
	}
	if res.Meta == nil {
		t.Fatal("应返回结构化结果")
	}
}

// TestEmptyZipPathRejected 空路径显式拒绝。
func TestEmptyZipPathRejected(t *testing.T) {
	h := plugintest.New(t)
	if res := invoke(t, h, "import_session", `{"zip_path":" "}`); !res.IsError {
		t.Fatal("空路径应拒绝")
	}
}
