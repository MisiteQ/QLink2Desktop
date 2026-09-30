// Package webui 内嵌前端资源，保证编译产物是一个零外部依赖的单文件。
//
// 前端采用「原生 ES 模块 + 无构建步骤」的组织方式：
//   - 不引入 npm / bundler，`go build` 一步出可执行文件，CI 与本地一致；
//   - 模块之间用 import 显式声明依赖，浏览器原生解析，改完刷新即可；
//   - 代价是需要现代浏览器，而飞牛桌面与主流浏览器都满足这个前提。
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:web
var files embed.FS

// FS 返回以 web/ 为根的文件系统：
//
//	index.html
//	assets/app.js
//	assets/style.css
//	assets/modules/*.js
func FS() fs.FS {
	sub, err := fs.Sub(files, "web")
	if err != nil {
		// 正常构建下不会发生；真出问题也不能让进程起不来，
		// 返回原始 FS 会让 404 而不是 panic。
		return files
	}
	return sub
}
