package eval

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// 每个测试进程只构建一次；从包的工作目录向上找模块，兼容 worktree 与 -trimpath。
func TestMain(m *testing.M) {
	code := func() int {
		root, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for {
			if _, err := os.Stat(filepath.Join(root, "apps", "xinli", "go.mod")); err == nil {
				break
			}
			parent := filepath.Dir(root)
			if parent == root {
				fmt.Fprintln(os.Stderr, "cannot locate apps/xinli from test working directory")
				return 1
			}
			root = parent
		}
		dir, err := os.MkdirTemp("", "yanshi-eval-xinli-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		defaultXinliBin = filepath.Join(dir, "xinli")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "build", "-o", defaultXinliBin, "./cmd/xinli")
		cmd.Dir = filepath.Join(root, "apps", "xinli")
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "build xinli fixture:", err)
			return 1
		}
		code := m.Run()
		if code == 0 {
			if err := writeCaptureGolden(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		return code
	}()
	os.Exit(code)
}
