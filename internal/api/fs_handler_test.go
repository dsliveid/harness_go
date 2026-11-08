package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"harness-go/internal/apperr"
	"harness-go/internal/observ"
)

// newFSTestRouter 构造一个只带目录列举能力的路由（不依赖 store / service），
// 用于把安全守卫的边界逐条钉死。
//
// 令牌校验器固定放行：鉴权本身由中间件负责，这里有独立用例验证「不接受匿名访问」。
func newFSTestRouter(t *testing.T, roots []string) (*httptest.Server, string) {
	t.Helper()
	logger := observ.NewLogger("error", "text", io.Discard)
	allow := TokenVerifier(func(_ context.Context, token string) (string, error) {
		if token == "good" {
			return "user-1", nil
		}
		return "", apperr.New(apperr.CodeUnauthorized, "令牌无效")
	})
	h := NewRouter(Deps{
		Version:     "test",
		Log:         logger,
		FSRoots:     roots,
		AuthConfig:  AuthConfig{SessionTTL: 60 * 1e9},
		VerifyToken: allow,
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, "good"
}

// doFSGet 发起带令牌的 GET 请求，返回状态码与已解析的错误码（成功时 code 为空）。
func doFSGet(t *testing.T, srv *httptest.Server, token, url string) (int, string, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+url, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	code := ""
	var eb apperr.Body
	if json.Unmarshal(body, &eb) == nil {
		code = eb.Error.Code
	}
	return resp.StatusCode, code, body
}

// TestFSSecurityGuards 目录列举的安全守卫（越界 / 穿越 / 非目录 / 匿名）。
func TestFSSecurityGuards(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "ProjA")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	// 白名单之外的一个目录（用于验证越界拒绝）
	outside := t.TempDir()

	srv, token := newFSTestRouter(t, []string{root})

	t.Run("越界路径被拒", func(t *testing.T) {
		status, code, _ := doFSGet(t, srv, token, "/api/v1/fs/dirs?host=server&path="+urlEscape(outside))
		if status != http.StatusBadRequest || code != string(apperr.CodeFSPathDenied) {
			t.Fatalf("期望 400 fs.path_denied，得到 %d %q", status, code)
		}
	})

	t.Run("含上级引用被拒（Clean 抹平前拦截）", func(t *testing.T) {
		// 关键用例：Clean 会把 a/../../x 抹平，必须靠 Clean 之前的段判断拦住
		bad := filepath.Join(root, "ProjA", "..", "..", "etc")
		status, code, _ := doFSGet(t, srv, token, "/api/v1/fs/dirs?host=server&path="+urlEscape(bad))
		if status != http.StatusBadRequest || code != string(apperr.CodeFSPathDenied) {
			t.Fatalf("期望 400 fs.path_denied，得到 %d %q", status, code)
		}
	})

	t.Run("非绝对路径被拒", func(t *testing.T) {
		status, code, _ := doFSGet(t, srv, token, "/api/v1/fs/dirs?host=server&path=relative/dir")
		if status != http.StatusBadRequest || code != string(apperr.CodeFSPathDenied) {
			t.Fatalf("期望 400 fs.path_denied，得到 %d %q", status, code)
		}
	})

	t.Run("指向文件返回 404", func(t *testing.T) {
		f := filepath.Join(root, "a.txt")
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatalf("写文件失败: %v", err)
		}
		status, code, _ := doFSGet(t, srv, token, "/api/v1/fs/dirs?host=server&path="+urlEscape(f))
		if status != http.StatusNotFound || code != string(apperr.CodeFSPathNotFound) {
			t.Fatalf("期望 404 fs.path_not_found，得到 %d %q", status, code)
		}
	})

	t.Run("匿名访问被拒", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/api/v1/fs/dirs?host=server&path=" + urlEscape(root))
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("期望 401，得到 %d", resp.StatusCode)
		}
	})
}

// TestFSListOnlyDirs 只列目录：文件与软链一律不出现。
func TestFSListOnlyDirs(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"alpha", "Beta", "gamma"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "readme.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	srv, token := newFSTestRouter(t, []string{root})
	status, code, body := doFSGet(t, srv, token, "/api/v1/fs/dirs?host=server&path="+urlEscape(root))
	if status != http.StatusOK {
		t.Fatalf("期望 200，得到 %d %q", status, code)
	}
	var resp fsDirsResp
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if len(resp.Dirs) != 3 {
		t.Fatalf("应只返回 3 个目录，得到 %d: %+v", len(resp.Dirs), resp.Dirs)
	}
	for _, d := range resp.Dirs {
		if strings.Contains(d.Name, ".") {
			t.Fatalf("文件项泄漏到目录列表: %q", d.Name)
		}
	}
	// 大小写不敏感排序（alpha / Beta / gamma）
	if resp.Dirs[0].Name != "alpha" || resp.Dirs[1].Name != "Beta" {
		t.Fatalf("排序不符合预期: %+v", resp.Dirs)
	}
}

// TestFSParentNilAtRoot 到达白名单根时 parent 必须为 null，绝不回 ".."。
func TestFSParentNilAtRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	srv, token := newFSTestRouter(t, []string{root})

	// 根本身：parent 必须为 null
	_, _, body := doFSGet(t, srv, token, "/api/v1/fs/dirs?host=server&path="+urlEscape(root))
	var atRoot fsDirsResp
	if err := json.Unmarshal(body, &atRoot); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if atRoot.Parent != nil {
		t.Fatalf("根层级 parent 必须为 null，得到 %q", *atRoot.Parent)
	}

	// 子目录：parent 指回根
	_, _, body = doFSGet(t, srv, token, "/api/v1/fs/dirs?host=server&path="+urlEscape(filepath.Join(root, "sub")))
	var inSub fsDirsResp
	if err := json.Unmarshal(body, &inSub); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if inSub.Parent == nil || *inSub.Parent != root {
		t.Fatalf("子目录 parent 应为根 %q，得到 %v", root, inSub.Parent)
	}
}

// TestFSDeviceHostUnsupported 本期（L0）客户端宿主一律 host_unsupported，绝不降级为服务端列举。
func TestFSDeviceHostUnsupported(t *testing.T) {
	root := t.TempDir()
	srv, token := newFSTestRouter(t, []string{root})

	for _, url := range []string{
		"/api/v1/fs/roots?host=device:dev-abc",
		"/api/v1/fs/dirs?host=device:dev-abc&path=" + urlEscape(root),
	} {
		status, code, _ := doFSGet(t, srv, token, url)
		if status != http.StatusConflict || code != string(apperr.CodeFSHostUnsupported) {
			t.Fatalf("%s 期望 409 fs.host_unsupported，得到 %d %q", url, status, code)
		}
	}
}

// TestFSNoRootsConfigured 未配置白名单时明确报错，绝不放行全盘。
func TestFSNoRootsConfigured(t *testing.T) {
	srv, token := newFSTestRouter(t, nil)
	status, code, _ := doFSGet(t, srv, token, "/api/v1/fs/roots?host=server")
	if status != http.StatusConflict || code != string(apperr.CodeFSHostUnsupported) {
		t.Fatalf("期望 409 fs.host_unsupported，得到 %d %q", status, code)
	}
}

// TestFSRootsListing 根列表返回白名单根，且带平台分隔符。
func TestFSRootsListing(t *testing.T) {
	root := t.TempDir()
	srv, token := newFSTestRouter(t, []string{root})
	status, code, body := doFSGet(t, srv, token, "/api/v1/fs/roots?host=server")
	if status != http.StatusOK {
		t.Fatalf("期望 200，得到 %d %q", status, code)
	}
	var resp fsRootsResp
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(resp.Roots) != 1 || resp.Roots[0].Path != filepath.Clean(root) {
		t.Fatalf("根列表不符预期: %+v", resp.Roots)
	}
	want := "/"
	if runtime.GOOS == "windows" {
		want = `\`
	}
	if resp.Separator != want {
		t.Fatalf("分隔符应为 %q，得到 %q", want, resp.Separator)
	}
}

// urlEscape 把路径转成可直接拼接的 query 值（Windows 反斜杠与盘符冒号必须编码）。
func urlEscape(p string) string {
	return strings.NewReplacer(
		"\\", "%5C",
		":", "%3A",
		" ", "%20",
	).Replace(p)
}
