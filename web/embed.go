// Package webdist 内嵌前端构建产物（web/dist），由 internal/api 的静态服务读取。
//
// 设计要点（阶段 B-1）：
//   - `//go:embed all:dist` 在**编译期**要求 dist 目录存在，故仓库内保留 web/dist/.gitkeep 占位；
//   - 未构建前端（dist 只有占位文件）时，Built() 返回 false，静态服务返回「前端未构建」提示页，
//     保证 `go build` / `go test ./...` 在纯后端环境下依然可用；
//   - 禁止在此包引入任何第三方依赖（保持单二进制纯净）。
package webdist

import "embed"

// Root 前端产物在 Assets 中的根目录名。
const Root = "dist"

//go:embed all:dist
var Assets embed.FS

// Built 报告前端是否已构建（以 index.html 是否存在为判据）。
func Built() bool {
	_, err := Assets.Open(Root + "/index.html")
	return err == nil
}
