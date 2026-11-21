package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"yanshi/internal/artifact/fsblob"
	"yanshi/internal/sandbox"
	"yanshi/internal/sandbox/sandboxtest"
)

// fakeExec 模拟 apiserver 的 exec（v5.channel.k8s.io）：把 stdin 原样写到 stdout，stderr 写 "e"，退出码 3。
func fakeExec(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/pods/p1/exec") || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "bad request", 400)
			return
		}
		if got := r.URL.Query()["command"]; strings.Join(got, " ") != "cat -" {
			http.Error(w, "bad command", 400)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{execProtocol}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		var in bytes.Buffer
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if data[0] == chClose && data[1] == chStdin {
				break
			}
			in.Write(data[1:])
		}
		_ = conn.Write(ctx, websocket.MessageBinary, append([]byte{chStdout}, in.Bytes()...))
		_ = conn.Write(ctx, websocket.MessageBinary, []byte{chStderr, 'e'})
		st, _ := json.Marshal(map[string]any{"status": "Failure", "reason": "NonZeroExitCode",
			"details": map[string]any{"causes": []any{map[string]any{"reason": "ExitCode", "message": "3"}}}})
		_ = conn.Write(ctx, websocket.MessageBinary, append([]byte{chError}, st...))
		conn.Close(websocket.StatusNormalClosure, "")
	}))
}

func TestExecProtocol(t *testing.T) {
	srv := fakeExec(t)
	defer srv.Close()
	tok := t.TempDir() + "/token"
	if err := os.WriteFile(tok, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(Config{API: srv.URL, TokenFile: tok, Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	in := bytes.Repeat([]byte("abc"), 100_000) // 跨越多个 stdin 分片
	var out, errb bytes.Buffer
	code, err := c.exec(context.Background(), "p1", "sandbox", []string{"cat", "-"}, bytes.NewReader(in), &out, &errb)
	if err != nil || code != 3 || !bytes.Equal(out.Bytes(), in) || errb.String() != "e" {
		t.Fatalf("code %d err %v out %d bytes stderr %q", code, err, out.Len(), errb.String())
	}
}

func TestExitCode(t *testing.T) {
	ok := &status{Status: "Success"}
	if n, err := exitCode(ok); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	if _, err := exitCode(nil); err == nil {
		t.Fatal("missing status must be an error")
	}
	other := &status{Status: "Failure", Reason: "InternalError", Message: "container not found"}
	if _, err := exitCode(other); err == nil || !strings.Contains(err.Error(), "container not found") {
		t.Fatalf("err = %v", err)
	}
}

// 沙箱 Pod 的安全配置：不挂载令牌、不注入服务地址、非 root、只读根文件系统、丢弃全部 capability、按配置选运行时。
func TestPodSpec(t *testing.T) {
	p := &Provider{Image: "img", RuntimeClass: "gvisor"}
	p.defaults()
	b, _ := json.Marshal(p.podSpec("sbx_ses_ABC"))
	s := string(b)
	for _, want := range []string{
		`"automountServiceAccountToken":false`, `"enableServiceLinks":false`, `"runtimeClassName":"gvisor"`,
		`"runAsNonRoot":true`, `"readOnlyRootFilesystem":true`, `"allowPrivilegeEscalation":false`, `"drop":["ALL"]`,
		`"seccompProfile":{"type":"RuntimeDefault"}`, `"yanshi.io/sandbox-id":"sbx_ses_ABC"`, `"restartPolicy":"Never"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("pod spec missing %s:\n%s", want, s)
		}
	}
	if n := podName("sbx_ses_ABC"); len(n) > 63 || strings.ToLower(n) != n || n == podName("sbx_ses_abc") {
		t.Fatalf("pod name %q", n)
	}
}

func TestLimitReader(t *testing.T) {
	lr := &limitReader{r: strings.NewReader(strings.Repeat("x", 100)), left: 10}
	_, err := io.ReadAll(lr)
	if !errors.Is(err, sandbox.ErrWorkspaceTooLarge) || !lr.exceeded {
		t.Fatalf("err %v exceeded %v", err, lr.exceeded)
	}
	lr = &limitReader{r: strings.NewReader("short"), left: 10}
	if b, err := io.ReadAll(lr); err != nil || string(b) != "short" {
		t.Fatal(b, err)
	}
}

// 集成测试需要一个装有隔离运行时的集群（docs/design/m2-sandbox.md §7）：
//
//	YANSHI_TEST_K8S=1 YANSHI_TEST_K8S_API=https://127.0.0.1:6443 YANSHI_TEST_K8S_TOKEN_FILE=… YANSHI_TEST_K8S_CA_FILE=…
//	YANSHI_TEST_K8S_NAMESPACE=yanshi-sandbox YANSHI_TEST_K8S_RUNTIME_CLASS=gvisor go test ./internal/sandbox/k8s
func testProvider(t *testing.T, limit int64) *Provider {
	if os.Getenv("YANSHI_TEST_K8S") == "" {
		t.Skip("YANSHI_TEST_K8S not set")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	c, err := NewClient(Config{API: os.Getenv("YANSHI_TEST_K8S_API"), TokenFile: os.Getenv("YANSHI_TEST_K8S_TOKEN_FILE"),
		CAFile: os.Getenv("YANSHI_TEST_K8S_CA_FILE"), Namespace: env("YANSHI_TEST_K8S_NAMESPACE", "yanshi-sandbox")})
	if err != nil {
		t.Fatal(err)
	}
	return &Provider{Client: c, Image: env("YANSHI_TEST_K8S_IMAGE", "python:3.12-slim"), RuntimeClass: os.Getenv("YANSHI_TEST_K8S_RUNTIME_CLASS"),
		MemoryMB: 256, CPUs: 0.5, WorkspaceLimit: limit, Blobs: fsblob.Store{Dir: t.TempDir()}}
}

func newID(t *testing.T) string {
	return "test-" + strings.ReplaceAll(t.Name(), "/", "-") + "-" + time.Now().Format("150405.000000")
}

func TestProvider(t *testing.T) {
	sandboxtest.RunProvider(t, testProvider(t, 0), newID)
}

func TestNetworkPolicyPresent(t *testing.T) {
	p := testProvider(t, 0)
	if err := p.Client.CheckNetworkPolicy(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// 工作区超过上限：Stop 不释放、不覆盖上一次保存，后续执行提示模型；清理后 Stop 正常。
func TestWorkspaceTooLarge(t *testing.T) {
	ctx := context.Background()
	p := testProvider(t, 256<<10)
	id := newID(t)
	t.Cleanup(func() { _ = p.Destroy(context.Background(), id) })
	run := func(script string) *sandbox.ExecResult {
		t.Helper()
		res, err := p.Exec(ctx, id, sandbox.ExecRequest{Argv: []string{"sh", "-c", script}})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("%s: %+v %v", script, res, err)
		}
		return res
	}
	if err := p.Ensure(ctx, id); err != nil {
		t.Fatal(err)
	}
	run("echo keep > keep.txt")
	if err := p.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(ctx, id); err != nil {
		t.Fatal(err)
	}
	run("head -c 1048576 /dev/urandom > big.bin && echo changed > keep.txt")
	if err := p.Stop(ctx, id); !errors.Is(err, sandbox.ErrWorkspaceTooLarge) {
		t.Fatalf("stop = %v", err)
	}
	if err := p.Ensure(ctx, id); err != nil { // Pod 仍在运行，改动没有丢
		t.Fatal(err)
	}
	if res := run("cat keep.txt"); string(res.Stdout) != "changed\n" || !strings.Contains(string(res.Stderr), "工作区超过") {
		t.Fatalf("after oversize: %+v", res)
	}
	run("rm big.bin")
	if err := p.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(ctx, id); err != nil {
		t.Fatal(err)
	}
	if res := run("cat keep.txt"); string(res.Stdout) != "changed\n" || strings.Contains(string(res.Stderr), "工作区超过") {
		t.Fatalf("after cleanup: %+v", res)
	}
}

// Pod 在 Stop 之外消失（驱逐、节点故障）：Ensure 重建并恢复上一次保存的工作区。
func TestPodLostRestoresLastSave(t *testing.T) {
	ctx := context.Background()
	p := testProvider(t, 0)
	id := newID(t)
	t.Cleanup(func() { _ = p.Destroy(context.Background(), id) })
	if err := p.Ensure(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, id, sandbox.ExecRequest{Argv: []string{"sh", "-c", "echo saved > a.txt"}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, id, sandbox.ExecRequest{Argv: []string{"sh", "-c", "echo unsaved > b.txt"}}); err != nil {
		t.Fatal(err)
	}
	if err := p.delete(ctx, podName(id)); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(ctx, id); err != nil {
		t.Fatal(err)
	}
	res, err := p.Exec(ctx, id, sandbox.ExecRequest{Argv: []string{"sh", "-c", "cat a.txt; ls"}})
	if err != nil || string(res.Stdout) != "saved\na.txt\n" {
		t.Fatalf("after pod loss: %q %v", res.Stdout, err)
	}
}
