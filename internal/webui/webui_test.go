package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRoutes：控制台的页面、脚本按路径提供，脚本以 JavaScript 类型返回（浏览器据此加载 ES 模块）；旧的通话页重定向到控制台。
func TestRoutes(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	for _, c := range []struct {
		path, contains, typ string
		code                int
	}{
		{"/call", "yanshi 调试控制台", "text/html", 200},
		{"/console", "yanshi 调试控制台", "text/html", 200},
		{"/console/", "yanshi 调试控制台", "text/html", 200},
		{"/console/app.js", "runScenario", "text/javascript", 200},
		{"/console/device.js", "class SimDevice", "text/javascript", 200},
		{"/console/chat.js", "class ChatPanel", "text/javascript", 200},
		{"/console/cards.js", "renderCard", "text/javascript", 200},
		{"/console/scenarios.js", "export const SCENARIOS", "text/javascript", 200},
		{"/console/console.css", ".ycard", "text/css", 200},
		{"/sdk/yanshi.js", "export", "text/javascript", 200},
		{"/console/missing.js", "", "", 404},
		{"/other", "", "", 404},
	} {
		res, err := http.Get(srv.URL + c.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != c.code || !strings.Contains(string(body), c.contains) || !strings.HasPrefix(res.Header.Get("Content-Type"), c.typ) {
			t.Errorf("%s: %d %q, body contains %q: %v", c.path, res.StatusCode, res.Header.Get("Content-Type"), c.contains, strings.Contains(string(body), c.contains))
		}
	}
}
