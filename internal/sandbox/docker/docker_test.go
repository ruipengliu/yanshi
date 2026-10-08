package docker_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"yanshi/internal/sandbox"
	"yanshi/internal/sandbox/docker"
)

// 需要 Docker；make test 会设置 YANSHI_TEST_DOCKER=1。镜像较小，不含 pandas 等。
const testImage = "python:3.12-slim"

func provider(t *testing.T) (*docker.Provider, string) {
	if os.Getenv("YANSHI_TEST_DOCKER") == "" {
		t.Skip("YANSHI_TEST_DOCKER not set")
	}
	p := &docker.Provider{Image: testImage, MemoryMB: 256}
	id := "test-" + strings.ReplaceAll(t.Name(), "/", "-") + "-" + time.Now().Format("150405.000000")
	t.Cleanup(func() { _ = p.Destroy(context.Background(), id) })
	if err := p.Ensure(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return p, id
}

func TestExecAndWorkspacePersistsAcrossStop(t *testing.T) {
	ctx := context.Background()
	p, id := provider(t)
	res, err := p.Exec(ctx, id, sandbox.ExecRequest{Argv: []string{"python3", "-"}, Stdin: []byte("open('a.txt','w').write('hi'); print(6*7)")})
	if err != nil || res.ExitCode != 0 || strings.TrimSpace(string(res.Stdout)) != "42" {
		t.Fatalf("exec = %+v, %v", res, err)
	}
	if err := p.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(ctx, id); err != nil {
		t.Fatal(err)
	}
	data, _, err := p.ReadFile(ctx, id, "/workspace/a.txt", 100)
	if err != nil || string(data) != "hi" {
		t.Fatalf("workspace lost after stop: %q, %v", data, err)
	}
}

func TestIsolation(t *testing.T) {
	ctx := context.Background()
	p, id := provider(t)
	script := `
import os, socket
print("uid", os.getuid())
try:
    socket.create_connection(("1.1.1.1", 53), timeout=2); print("net ok")
except OSError: print("net blocked")
try:
    open("/usr/evil", "w"); print("rootfs writable")
except OSError: print("rootfs read-only")
`
	res, err := p.Exec(ctx, id, sandbox.ExecRequest{Argv: []string{"python3", "-"}, Stdin: []byte(script)})
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Stdout)
	for _, want := range []string{"uid 1000", "net blocked", "rootfs read-only"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s%s", want, out, res.Stderr)
		}
	}
}

func TestTimeoutKills(t *testing.T) {
	p, id := provider(t)
	start := time.Now()
	res, err := p.Exec(context.Background(), id, sandbox.ExecRequest{Argv: []string{"sleep", "30"}, Timeout: 2 * time.Second})
	if err != nil || !res.TimedOut || time.Since(start) > 15*time.Second {
		t.Fatalf("res = %+v, err %v, took %s", res, err, time.Since(start))
	}
}

func TestOutputTruncated(t *testing.T) {
	p, id := provider(t)
	res, err := p.Exec(context.Background(), id, sandbox.ExecRequest{Argv: []string{"python3", "-c", "print('x'*100000)"}})
	if err != nil || !res.StdoutTruncated || len(res.Stdout) != sandbox.OutputLimit {
		t.Fatalf("len %d truncated %v err %v", len(res.Stdout), res.StdoutTruncated, err)
	}
}
