package api

// 前端静态资源服务（阶段 B-1）：把 //go:embed 的 web/dist 暴露到 HTTP。
//
// 设计要点：
//   - **未构建前端时不能 500，也不能编译失败**：dist 里只有 .gitkeep 占位，
//     serveIndex 返回 200 的可读提示页，并只告警一次（避免日志刷屏）；
//   - 带 hash 的产物（/assets/*）用 immutable 长缓存，其余（manifest/sw/icons）no-cache；
//   - 目录请求一律 404（不提供目录索引，避免暴露构建布局）。

import (
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"

	webdist "harness-go/web"
)

// distNotBuiltHTML 前端未构建时的提示页（HTTP 200，可读、可操作）。
const distNotBuiltHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>harness-go · 前端未构建</title></head>
<body style="font-family:system-ui;padding:2rem;line-height:1.8;max-width:44rem">
<h1 style="margin:0 0 .5rem">harness-go 已启动</h1>
<p style="color:#8b949e;margin:0 0 1.5rem">服务端内核与 API 均可用，但前端产物 <code>web/dist</code> 尚未构建。</p>
<p>构建命令：</p>
<pre style="background:#161b22;color:#c9d1d9;padding:1rem;border-radius:8px;overflow:auto">cd web &amp;&amp; npm install &amp;&amp; npm run build
# 或在仓库根目录
make build-all</pre>
<p>构建完成后刷新本页即可进入界面。</p>
<hr style="border:none;border-top:1px solid #30363d;margin:1.5rem 0">
<p style="color:#8b949e">可用 <code>GET /healthz</code> 检查健康状态，或用
<code>Authorization: Bearer &lt;token&gt;</code> 调用 <code>/api/v1/*</code>。</p>
</body></html>`

// assets 内嵌前端产物读取器。
type assets struct {
	fsys     fs.FS
	built    bool
	log      *slog.Logger
	warnOnce sync.Once
}

// newAssets 构造静态资源读取器（基于编译期内嵌的 web/dist）。
func newAssets(log *slog.Logger) *assets {
	return newAssetsFS(webdist.Assets, webdist.Root, log)
}

// newAssetsFS 从任意文件系统的子目录构造读取器（root 为 nil 或读取失败视为未构建）。
//
// 抽出来是为了可测：单元测试用 fstest.MapFS 分别覆盖「未构建」与「已构建」两条路径，
// 不依赖真实构建产物。
func newAssetsFS(root fs.FS, dir string, log *slog.Logger) *assets {
	if root == nil {
		return &assets{log: log}
	}
	sub, err := fs.Sub(root, dir)
	if err != nil {
		return &assets{log: log}
	}
	built := false
	if _, statErr := fs.Stat(sub, "index.html"); statErr == nil {
		built = true
	}
	return &assets{fsys: sub, built: built, log: log}
}

// Built 报告前端产物是否可用（便于测试与启动日志）。
func (a *assets) Built() bool { return a != nil && a.built && a.fsys != nil }

// serveIndex 输出 SPA 入口（index.html），未构建时输出提示页。
func (a *assets) serveIndex(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if !a.Built() {
		a.warnOnce.Do(func() {
			if a.log != nil {
				a.log.Warn("前端未构建：/ 将返回构建提示页",
					"hint", "cd web && npm install && npm run build（或 make build-all）")
			}
		})
		_, _ = io.WriteString(w, distNotBuiltHTML)
		return
	}
	data, err := fs.ReadFile(a.fsys, "index.html")
	if err != nil {
		a.warnOnce.Do(func() {
			if a.log != nil {
				a.log.Warn("index.html 读取失败，返回构建提示页", "err", err)
			}
		})
		_, _ = io.WriteString(w, distNotBuiltHTML)
		return
	}
	_, _ = w.Write(data)
}

// serveStatic 输出静态文件（/assets/*、/manifest.webmanifest、/sw.js、/icons/*）。
func (a *assets) serveStatic(w http.ResponseWriter, r *http.Request) {
	if !a.Built() {
		http.NotFound(w, r)
		return
	}
	// 显式拒绝含 ".." 的原始路径：path.Clean 会把 "/../index.html" 归一成 "/index.html"，
	// 虽然不会越出 embed FS，但放过它就是放过穿越意图，直接 404 更保守。
	if strings.Contains(r.URL.Path, "..") {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" || name == "." {
		http.NotFound(w, r)
		return
	}
	if st, err := fs.Stat(a.fsys, name); err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(a.fsys, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentTypeFor(name))
	if strings.HasPrefix(name, "assets/") {
		// 带内容 hash 的构建产物：可长期强缓存
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	_, _ = w.Write(data)
}

// contentTypeFor 推断 MIME。
//
// 为什么不用 mime.TypeByExtension 兜底：Windows 注册表通常没有 .webmanifest 映射，
// 会退化成 application/octet-stream，导致浏览器拒绝解析 PWA manifest（控制台报错）。
func contentTypeFor(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".webmanifest":
		return "application/manifest+json; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".woff2":
		return "font/woff2"
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// serveLogin 登录页路由：与首页同为 SPA 入口（前端自行按路由渲染 LoginPage）。
func (h *handlers) serveLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	h.web.serveIndex(w)
}

// serveWebAsset 静态资源路由入口。
func (h *handlers) serveWebAsset(w http.ResponseWriter, r *http.Request) {
	h.web.serveStatic(w, r)
}
