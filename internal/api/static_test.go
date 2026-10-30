package api

// 前端静态服务测试（阶段 B-1）：
//   - 未构建（dist 只有占位）→ `/` 返回 200 提示页，静态资源 404，且不 panic；
//   - 已构建 → `/` 返回真实 index.html，`/assets/*` 返回正确 MIME 与长缓存，
//     manifest 用 application/manifest+json（Windows 上 mime 表缺该扩展名，曾退化成 octet-stream）；
//   - SPA 路由 `/login` 与 `/` 行为一致（都返回 HTML 入口）。

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func builtFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":              {Data: []byte(`<!doctype html><html><body><div id="root"></div></body></html>`)},
		"assets/index-abc123.js":  {Data: []byte(`console.log('harness')`)},
		"assets/index-abc123.css": {Data: []byte(`:root{--bg-rgb:13 17 23}`)},
		"manifest.webmanifest":    {Data: []byte(`{"name":"harness-go"}`)},
		"sw.js":                   {Data: []byte(`self.addEventListener('install',()=>{})`)},
		"icons/icon.svg":          {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
	}
}

func notBuiltFS() fstest.MapFS {
	return fstest.MapFS{
		".gitkeep": {Data: []byte("# placeholder\n")},
	}
}

func serve(t *testing.T, a *assets, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, nil)
	a.serveStatic(rec, req)
	return rec
}

func TestStaticNotBuiltServesHintPage(t *testing.T) {
	a := newAssetsFS(notBuiltFS(), ".", discardLogger())
	if a.Built() {
		t.Fatal("仅有占位文件时不应视为已构建")
	}

	rec := httptest.NewRecorder()
	a.serveIndex(rec)
	if rec.Code != http.StatusOK {
		t.Fatalf("未构建时 / 应返回 200 提示页，得到 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("提示页 Content-Type 异常: %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "前端未构建") {
		t.Fatalf("提示页应说明前端未构建，实际内容: %s", rec.Body.String())
	}

	// 静态资源在未构建时必须 404（而不是 500 或空 200）
	if got := serve(t, a, http.MethodGet, "/assets/index-abc123.js").Code; got != http.StatusNotFound {
		t.Fatalf("未构建时静态资源应 404，得到 %d", got)
	}
}

func TestStaticBuiltServesAppAndAssets(t *testing.T) {
	a := newAssetsFS(builtFS(), ".", discardLogger())
	if !a.Built() {
		t.Fatal("含 index.html 时应视为已构建")
	}

	// 入口
	rec := httptest.NewRecorder()
	a.serveIndex(rec)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("入口应返回 index.html，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("index.html 必须 no-store，得到 %q", rec.Header().Get("Cache-Control"))
	}

	// 带 hash 的产物：MIME + immutable 长缓存
	js := serve(t, a, http.MethodGet, "/assets/index-abc123.js")
	if js.Code != http.StatusOK {
		t.Fatalf("assets js 应 200，得到 %d", js.Code)
	}
	if ct := js.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("js MIME 异常: %q", ct)
	}
	if cc := js.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("assets 应长缓存，得到 %q", cc)
	}

	css := serve(t, a, http.MethodGet, "/assets/index-abc123.css")
	if ct := css.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("css MIME 异常: %q", ct)
	}

	// PWA manifest：必须 application/manifest+json，否则浏览器拒绝解析
	mf := serve(t, a, http.MethodGet, "/manifest.webmanifest")
	if mf.Code != http.StatusOK {
		t.Fatalf("manifest 应 200，得到 %d", mf.Code)
	}
	if ct := mf.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/manifest+json") {
		t.Fatalf("manifest MIME 必须为 application/manifest+json，得到 %q", ct)
	}
	if cc := mf.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("非 assets 资源应 no-cache，得到 %q", cc)
	}

	// 其余 PWA 资源
	if got := serve(t, a, http.MethodGet, "/sw.js").Code; got != http.StatusOK {
		t.Fatalf("sw.js 应 200，得到 %d", got)
	}
	if got := serve(t, a, http.MethodGet, "/icons/icon.svg").Code; got != http.StatusOK {
		t.Fatalf("icon.svg 应 200，得到 %d", got)
	}

	// 不存在的资源与目录请求都必须 404
	if got := serve(t, a, http.MethodGet, "/assets/nope.js").Code; got != http.StatusNotFound {
		t.Fatalf("缺失资源应 404，得到 %d", got)
	}
	if got := serve(t, a, http.MethodGet, "/assets/").Code; got != http.StatusNotFound {
		t.Fatalf("目录请求应 404，得到 %d", got)
	}
}

func TestStaticRejectsTraversal(t *testing.T) {
	a := newAssetsFS(builtFS(), ".", discardLogger())
	for _, target := range []string{"/../index.html", "/assets/../../etc/passwd"} {
		if got := serve(t, a, http.MethodGet, target).Code; got != http.StatusNotFound {
			t.Fatalf("路径穿越 %q 应 404，得到 %d", target, got)
		}
	}
}

// TestSPARoutesServeHTML 覆盖真实路由：`/`（走 SSO 拦截后回退）与 `/login` 都应返回 HTML。
//
// 不依赖是否已构建前端：两种状态下都必须是 200 + text/html（未构建是提示页）。
func TestSPARoutesServeHTML(t *testing.T) {
	h := &handlers{web: newAssetsFS(notBuiltFS(), ".", discardLogger())}

	for _, path := range []string{"/", "/login"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if path == "/" {
			h.root(rec, req)
		} else {
			h.serveLogin(rec, req)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 应 200，得到 %d", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("%s Content-Type 异常: %q", path, ct)
		}
		if rec.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s 必须带 Referrer-Policy: no-referrer", path)
		}
	}
}

// TestRootSSOStillIntercepts 确认接入前端后 SSO 拦截未被破坏（带凭据走 302）。
func TestRootSSOStillIntercepts(t *testing.T) {
	h := &handlers{web: newAssetsFS(notBuiltFS(), ".", discardLogger())}
	// SSO 关闭时：带 user_name 应返回 302（sso_error），而不是首页 HTML
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?user_name=admin", nil)
	h.root(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("带凭据但 SSO 关闭时应 302，得到 %d", rec.Code)
	}
}
