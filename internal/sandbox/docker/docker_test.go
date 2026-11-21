package docker_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"yanshi/internal/sandbox/docker"
	"yanshi/internal/sandbox/sandboxtest"
)

// 需要 Docker；make test 会设置 YANSHI_TEST_DOCKER=1。镜像较小，不含 pandas 等。
const testImage = "python:3.12-slim"

func TestProvider(t *testing.T) {
	if os.Getenv("YANSHI_TEST_DOCKER") == "" {
		t.Skip("YANSHI_TEST_DOCKER not set")
	}
	// YANSHI_TEST_DOCKER_RUNTIME 选择运行时（如 runsc），用于在隔离运行时下复核兼容性（ADR-0053）。
	p := &docker.Provider{Image: testImage, MemoryMB: 256, Runtime: os.Getenv("YANSHI_TEST_DOCKER_RUNTIME")}
	sandboxtest.RunProvider(t, p, func(t *testing.T) string {
		return "test-" + strings.ReplaceAll(t.Name(), "/", "-") + "-" + time.Now().Format("150405.000000")
	})
}
