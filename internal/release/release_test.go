package release

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestWatchFilesKeepsPreviousOnDirectoryError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second := t.TempDir(), t.TempDir()
		write := func(path, contents string) {
			t.Helper()
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		files := Files{}
		for name, dir := range map[string]string{"first": first, "second": second} {
			path := filepath.Join(dir, "releases.yaml")
			write(path, name+": {stable: '1'}\n")
			files = append(files, File{Path: path, Parse: func(b []byte) (Releases, error) {
				return Parse(b, func(n, version string) bool { return n == name && (version == "1" || version == "2") })
			}})
		}
		current, loaded, err := files.Load()
		if err != nil {
			t.Fatal(err)
		}
		// Watch 在自己的 goroutine 里替换配置、写日志；synctest.Wait 只保证它已阻塞，不构成竞态检测认可的同步，
		// 因此测试读取两者时须加锁。
		var mu sync.Mutex
		logs := &lockedBuffer{mu: &mu}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go files.Watch(ctx, loaded, time.Second, slog.New(slog.NewTextHandler(logs, nil)), func(rel Releases) {
			mu.Lock()
			defer mu.Unlock()
			current = rel
		})
		poll := func() {
			time.Sleep(time.Second)
			synctest.Wait()
		}
		assertVersions := func(first, second string) {
			t.Helper()
			mu.Lock()
			defer mu.Unlock()
			if current["first"].Stable != first || current["second"].Stable != second {
				t.Fatalf("releases = %v, want versions %q and %q", current, first, second)
			}
		}
		if err := os.Rename(second, second+"-unavailable"); err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(second + "-unavailable")
		write(filepath.Join(first, "releases.yaml"), "first: {stable: '2'}\n")
		poll()
		assertVersions("1", "1")
		if !strings.Contains(logs.String(), "level=ERROR") {
			t.Fatal("failed reload was not logged")
		}
		if err := os.Rename(second+"-unavailable", second); err != nil {
			t.Fatal(err)
		}
		secondPath := filepath.Join(second, "releases.yaml")
		write(secondPath, "second: {stable: '3'}\n")
		poll()
		assertVersions("1", "1")
		write(secondPath, "second: {stable: '2'}\n")
		poll()
		assertVersions("2", "2")
		if err := os.Remove(secondPath); err != nil {
			t.Fatal(err)
		}
		poll()
		assertVersions("2", "")
	})
}

// lockedBuffer 与测试中的配置共用一把锁，让测试读日志时与 Watch 的写入互斥。
type lockedBuffer struct {
	mu  *sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
