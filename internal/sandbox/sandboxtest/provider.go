package sandboxtest

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"yanshi/internal/sandbox"
)

// RunProvider 是 sandbox.Provider 实现的一致性套件：执行、工作区在 Stop 后保留、隔离配置、
// 沙箱内超时、输出截断、文件读写与幂等的 Destroy。newID 返回一个从未用过的沙箱 ID；
// 套件负责 Ensure 与清理。镜像须含 python3 与 coreutils。
func RunProvider(t *testing.T, p sandbox.Provider, newID func(t *testing.T) string) {
	ensure := func(t *testing.T) string {
		t.Helper()
		id := newID(t)
		t.Cleanup(func() { _ = p.Destroy(context.Background(), id) })
		if err := p.Ensure(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	t.Run("ExecAndWorkspacePersistsAcrossStop", func(t *testing.T) {
		ctx := context.Background()
		id := ensure(t)
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
		// 第二次 Stop / Ensure：恢复后的工作区仍可再次保存（防止只保存一次）。
		if _, err := p.Exec(ctx, id, sandbox.ExecRequest{Argv: []string{"sh", "-c", "echo again > b.txt"}}); err != nil {
			t.Fatal(err)
		}
		if err := p.Stop(ctx, id); err != nil {
			t.Fatal(err)
		}
		if err := p.Ensure(ctx, id); err != nil {
			t.Fatal(err)
		}
		res, err = p.Exec(ctx, id, sandbox.ExecRequest{Argv: []string{"cat", "a.txt", "b.txt"}})
		if err != nil || string(res.Stdout) != "hiagain\n" {
			t.Fatalf("after second stop: %+v, %v", res, err)
		}
	})

	t.Run("Isolation", func(t *testing.T) {
		id := ensure(t)
		script := `
import os, socket
print("uid", os.getuid())
try:
    socket.create_connection(("1.1.1.1", 53), timeout=2); print("net ok")
except OSError: print("net blocked")
try:
    open("/usr/evil", "w"); print("rootfs writable")
except OSError: print("rootfs read-only")
print("token", os.path.exists("/var/run/secrets/kubernetes.io/serviceaccount/token"))
print("cwd", os.getcwd())
`
		res, err := p.Exec(context.Background(), id, sandbox.ExecRequest{Argv: []string{"python3", "-"}, Stdin: []byte(script)})
		if err != nil {
			t.Fatal(err)
		}
		out := string(res.Stdout)
		for _, want := range []string{"uid 1000", "net blocked", "rootfs read-only", "token False", "cwd /workspace"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in:\n%s%s", want, out, res.Stderr)
			}
		}
	})

	t.Run("ExitCode", func(t *testing.T) {
		id := ensure(t)
		res, err := p.Exec(context.Background(), id, sandbox.ExecRequest{Argv: []string{"sh", "-c", "echo out; echo err >&2; exit 3"}})
		if err != nil || res.ExitCode != 3 || string(res.Stdout) != "out\n" || string(res.Stderr) != "err\n" || res.TimedOut {
			t.Fatalf("res = %+v, err %v", res, err)
		}
	})

	t.Run("TimeoutKills", func(t *testing.T) {
		id := ensure(t)
		start := time.Now()
		res, err := p.Exec(context.Background(), id, sandbox.ExecRequest{Argv: []string{"sleep", "30"}, Timeout: 2 * time.Second})
		if err != nil || !res.TimedOut || time.Since(start) > 15*time.Second {
			t.Fatalf("res = %+v, err %v, took %s", res, err, time.Since(start))
		}
	})

	t.Run("OutputTruncated", func(t *testing.T) {
		id := ensure(t)
		res, err := p.Exec(context.Background(), id, sandbox.ExecRequest{Argv: []string{"python3", "-c", "print('x'*100000)"}})
		if err != nil || !res.StdoutTruncated || len(res.Stdout) != sandbox.OutputLimit {
			t.Fatalf("len %d truncated %v err %v", len(res.Stdout), res.StdoutTruncated, err)
		}
	})

	t.Run("CopyInOut", func(t *testing.T) {
		ctx := context.Background()
		id := ensure(t)
		data := bytes.Repeat([]byte("0123456789"), 300_000) // 3MB：跨越多个传输分片
		if err := p.CopyIn(ctx, id, "/workspace/dir/sub/f.bin", bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := p.CopyOut(ctx, id, "/workspace/dir/sub/f.bin", &out); err != nil || !bytes.Equal(out.Bytes(), data) {
			t.Fatalf("copy out: %d bytes, %v", out.Len(), err)
		}
		if err := p.CopyOut(ctx, id, "/workspace/missing", &out); err == nil {
			t.Fatal("copy out of a missing file should fail")
		}
		got, truncated, err := p.ReadFile(ctx, id, "/workspace/dir/sub/f.bin", 10)
		if err != nil || !truncated || string(got) != "0123456789" {
			t.Fatalf("read file: %q %v %v", got, truncated, err)
		}
	})

	// Janitor 的删除会清扫两次，沙箱也可能已被空闲回收或从未创建：
	// 对不存在的沙箱与工作区，Destroy 视为已完成，而不是报错使删除无法结束。
	t.Run("DestroyIsIdempotent", func(t *testing.T) {
		ctx := context.Background()
		id := ensure(t)
		if err := p.Stop(ctx, id); err != nil {
			t.Fatal(err)
		}
		for i := range 2 {
			if err := p.Destroy(ctx, id); err != nil {
				t.Fatalf("destroy #%d: %v", i+1, err)
			}
		}
		if err := p.Destroy(ctx, newID(t)); err != nil {
			t.Fatalf("destroy of a sandbox that never existed: %v", err)
		}
		// 销毁后工作区不复存在：重新 Ensure 得到空工作区。
		if err := p.Ensure(ctx, id); err != nil {
			t.Fatal(err)
		}
		res, err := p.Exec(ctx, id, sandbox.ExecRequest{Argv: []string{"ls", "-A"}})
		if err != nil || len(bytes.TrimSpace(res.Stdout)) != 0 {
			t.Fatalf("workspace survived destroy: %q, %v", res.Stdout, err)
		}
	})

	t.Run("StopOfMissingIsNoop", func(t *testing.T) {
		if err := p.Stop(context.Background(), newID(t)); err != nil {
			t.Fatalf("stop of a sandbox that never existed: %v", err)
		}
	})
}
