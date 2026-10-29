package xinlifixture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// 用当前测试二进制模拟启动协议，不需要额外构建应用；各错误路径都应关闭 stdin 并回收进程。
func TestMain(m *testing.M) {
	mode := os.Getenv("YANSHI_FIXTURE_HELPER")
	if mode == "" {
		os.Exit(m.Run())
	}
	if mode == "storage" {
		kind := ""
		for i, arg := range os.Args {
			if arg == "-persona-dsn" {
				os.Exit(3)
			}
			if arg == "-store" && i+1 < len(os.Args) {
				kind = os.Args[i+1]
			}
		}
		if kind+"|"+os.Getenv("YANSHI_PERSONA_PG_DSN") != os.Getenv("YANSHI_FIXTURE_EXPECT_STORAGE") {
			os.Exit(4)
		}
		mode = "ready"
	}
	switch mode {
	case "bad-json":
		fmt.Println("not JSON")
	case "bad-registration":
		fmt.Println(`{"fixture_url":"http://127.0.0.1:1","registrations":[{"name":"bad"}]}`)
	case "bad-url":
		fmt.Println(`{"fixture_url":"not-http","registrations":[]}`)
	case "empty":
		fmt.Println(`{"fixture_url":"http://127.0.0.1:1","registrations":[]}`)
	case "exit":
		os.Exit(2)
	case "silent":
	case "ready", "ignore-eof":
		fmt.Println(`{"fixture_url":"http://127.0.0.1:1","registrations":[{"name":"xinli","business_line":"eval","url":"http://127.0.0.1:1/mcp"}]}`)
	}
	if mode == "ignore-eof" {
		time.Sleep(time.Minute)
	} else {
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
	os.Exit(0)
}

func helperBinary(t *testing.T, mode string) string {
	t.Helper()
	t.Setenv("YANSHI_FIXTURE_HELPER", mode)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestReadinessErrors(t *testing.T) {
	for _, mode := range []string{"bad-json", "bad-registration", "bad-url", "empty", "exit"} {
		t.Run(mode, func(t *testing.T) {
			client, _, err := Start(context.Background(), helperBinary(t, mode), "eval")
			if client != nil || err == nil || !strings.Contains(err.Error(), "readiness") {
				t.Fatalf("start: %v, %v", client, err)
			}
		})
	}
}

func TestReadinessCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	client, _, err := Start(ctx, helperBinary(t, "silent"), "eval")
	if client != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("start: %v, %v", client, err)
	}
}

func TestCancelReapsProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, _, err := Start(ctx, helperBinary(t, "ready"), "eval")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	cancel()
	select {
	case <-client.done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not reap child")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if client.cmd.ProcessState == nil || !client.cmd.ProcessState.Exited() {
		t.Fatal("child was not waited for")
	}
}

func TestCloseKillsUnresponsiveProcess(t *testing.T) {
	client, _, err := Start(context.Background(), helperBinary(t, "ignore-eof"), "eval")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Close(); err == nil {
		t.Fatal("expected killed process status")
	}
	select {
	case <-client.done:
	default:
		t.Fatal("Close returned without waiting")
	}
}

func TestStorageOptions(t *testing.T) {
	for _, tc := range []struct {
		name, store, dsn, inherited, want string
	}{
		{name: "default", want: "memory|"},
		{name: "pg_env", store: "pg", inherited: "postgres://fixture-only/env", want: "pg|postgres://fixture-only/env"},
		{name: "pg_override", store: "pg", dsn: "postgres://fixture-only/explicit", inherited: "postgres://fixture-only/env", want: "pg|postgres://fixture-only/explicit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("YANSHI_PERSONA_PG_DSN", tc.inherited)
			t.Setenv("YANSHI_FIXTURE_EXPECT_STORAGE", tc.want)
			client, _, err := StartWithOptions(context.Background(), helperBinary(t, "storage"), Options{Store: tc.store, PersonaDSN: tc.dsn}, "eval")
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInvalidStorageOptions(t *testing.T) {
	t.Setenv("YANSHI_PERSONA_PG_DSN", "")
	t.Setenv("YANSHI_PG_DSN", "postgres://kernel-only/yanshi")
	for _, opts := range []Options{{Store: "invalid"}, {Store: "pg"}} {
		client, _, err := StartWithOptions(context.Background(), "unused", opts, "eval")
		if client != nil || err == nil || !strings.Contains(err.Error(), "xinli fixture:") {
			t.Fatalf("invalid options: %v %v", client, err)
		}
	}
}
