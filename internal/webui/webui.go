// Package webui 是 serve 提供的网页：
//   - /call：通话页（docs/design/m3-call.md §8），用网页 SDK（sdk/web）经 Connection 接入，开始 Call、显示对话与
//     后台任务、处理审批与提问；
//   - /console：调试控制台（docs/design/console.md），在一个页面里模拟多台设备（Node 与会话客户端）接入网关，
//     观察每一帧、每个事件与投影，并运行验证场景。
//
// 二者都是开发与演示用的客户端，只使用公开的 Connection 协议与 HTTP API，不比其他客户端多任何权限；业务线的 App
// 自行实现界面。
//
// static/yanshi.js 是 sdk/web 的打包产物（cd sdk/web && pnpm bundle），随代码提交；make check 检查它与源码一致。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var static embed.FS

// Handler 提供 /call 与 /console 及其脚本。
func Handler() http.Handler {
	sub, _ := fs.Sub(static, "static")
	files := http.FileServer(http.FS(sub))
	callFiles := http.StripPrefix("/call", files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 页面与脚本不缓存：开发中频繁更新，体积也小。
		w.Header().Set("Cache-Control", "no-store")
		switch p := r.URL.Path; {
		case p == "/call" || p == "/call/":
			http.ServeFileFS(w, r, sub, "call.html")
		case p == "/console" || p == "/console/":
			http.ServeFileFS(w, r, sub, "console/index.html")
		case strings.HasPrefix(p, "/call/"):
			callFiles.ServeHTTP(w, r)
		case strings.HasPrefix(p, "/console/"):
			files.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}
