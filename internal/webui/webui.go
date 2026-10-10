// Package webui 是 serve 提供的网页通话页（/call，docs/design/m3-call.md §8）：用网页 SDK（sdk/web）经 Connection
// 接入，开始 Call、显示对话与后台任务、处理审批与提问。它是开发与演示用的客户端，业务线的 App 自行实现界面。
//
// static/yanshi.js 是 sdk/web 的打包产物（cd sdk/web && pnpm bundle），随代码提交；make check 检查它与源码一致。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var static embed.FS

// Handler 在 /call 提供通话页及其脚本。
func Handler() http.Handler {
	sub, _ := fs.Sub(static, "static")
	files := http.StripPrefix("/call/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 页面与脚本不缓存：开发中频繁更新，体积也小。
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/call" || r.URL.Path == "/call/" {
			http.ServeFileFS(w, r, sub, "call.html")
			return
		}
		files.ServeHTTP(w, r)
	})
}
