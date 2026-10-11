package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestConsoleScenarios：调试控制台（/console）的验证场景对真实网关全部通过。场景以 Node.js 运行
// （internal/webui/testdata/run-scenarios.mjs），用控制台的模拟设备经 protojson 文本帧接入：多端同步、晚加入者、
// 断线续传、输入去重、能力路由、审批、离线挂起、崩溃后重新投递、提问、插话与中断、在场、隔离。需要 YANSHI_TEST_NODE。
func TestConsoleScenarios(t *testing.T) {
	if os.Getenv("YANSHI_TEST_NODE") == "" {
		t.Skip("YANSHI_TEST_NODE not set")
	}
	script, err := filepath.Abs("../webui/testdata/run-scenarios.mjs")
	if err != nil {
		t.Fatal(err)
	}
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--disable-warning=MODULE_TYPELESS_PACKAGE_JSON", script)
	cmd.Env = append(os.Environ(), "YANSHI_URL="+e.connURL(), "YANSHI_HTTP="+e.srv.URL, "YANSHI_AGENT=dev",
		"YANSHI_USER_A=ca", "YANSHI_TOKEN_A="+mustSign("ca"), "YANSHI_USER_B=cb", "YANSHI_TOKEN_B="+mustSign("cb"))
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(out)), "ok") {
		t.Fatalf("console scenarios failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}
