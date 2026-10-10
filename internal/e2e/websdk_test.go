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

// TestWebSDK：TypeScript SDK（sdk/web）与真实网关互通。网关在本进程中启动，
// 互通脚本（sdk/web/test/interop.ts）以 Node.js 运行：两台网页设备实时增量与晚加入者 Snapshot、在场、
// ask_user 跨设备回答、输入去重、访问他人 Session。需要 YANSHI_TEST_NODE 且已构建 sdk/web（make check 会构建）。
func TestWebSDK(t *testing.T) {
	if os.Getenv("YANSHI_TEST_NODE") == "" {
		t.Skip("YANSHI_TEST_NODE not set")
	}
	script, err := filepath.Abs("../../sdk/web/dist/test/interop.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("build the web SDK first (cd sdk/web && pnpm test): %v", err)
	}
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", script)
	cmd.Env = append(os.Environ(), "YANSHI_URL="+e.connURL(), "YANSHI_TOKEN="+mustSign("u"), "YANSHI_OTHER_TOKEN="+mustSign("v"))
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(out)), "ok") {
		t.Fatalf("interop failed: %v\n%s", err, out)
	}
}
