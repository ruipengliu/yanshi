// Package webui 是 serve 提供的网页：/console 调试控制台（docs/design/console.md）。它在一个页面里模拟多台设备
// （Node 与会话客户端）接入网关：对话（语音、图片、卡片、通话）、设备（在线状态、模拟应用、能力的执行方式）、
// 控制台（时间线、事件、帧、故障注入、指标、验证场景）。
//
// 控制台是开发与演示用的客户端，只使用公开的 Connection 协议与 HTTP API；唯一的调试专用接口是 serve -faults 时的
// /v1/debug/*（ADR-0033）。业务线的 App 自行实现界面。
//
// /sdk/yanshi.js 是 sdk/web 的打包产物（cd sdk/web && pnpm bundle），供在浏览器中直接试用网页 SDK，随代码提交；
// make check 检查它与源码一致。原来的通话页 /call 已并入控制台的对话视图，旧链接重定向到 /console。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var static embed.FS

// Handler 提供 /console 及其脚本，以及 /sdk/yanshi.js。
func Handler() http.Handler {
	sub, _ := fs.Sub(static, "static")
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 页面与脚本不缓存：开发中频繁更新，体积也小。
		w.Header().Set("Cache-Control", "no-store")
		switch p := r.URL.Path; {
		case p == "/console" || p == "/console/":
			http.ServeFileFS(w, r, sub, "console/index.html")
		case strings.HasPrefix(p, "/console/"):
			files.ServeHTTP(w, r)
		case p == "/sdk/yanshi.js":
			http.ServeFileFS(w, r, sub, "yanshi.js")
		case p == "/call" || p == "/call/":
			http.Redirect(w, r, "/console", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	})
}
