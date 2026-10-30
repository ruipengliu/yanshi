package bff_test

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"persona/query"
	"xinli/task"
)

// 持锁方与排队方都是正常操作；超过两秒的等待不能被归为不可达。
func TestOwnerGateSlowRequestsQueue(t *testing.T) {
	for _, kind := range []string{"memory", "pg"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, kind)
			entered, release, done := make(chan struct{}), make(chan struct{}, 1), make(chan error, 1)
			go func() {
				done <- h.accounts.WithToken(context.Background(), h.a.AccessToken, func(context.Context, task.Owner, string) error { close(entered); <-release; return nil })
			}()
			released := false
			defer func() {
				if !released {
					close(release)
					if err := <-done; err != nil {
						t.Error(err)
					}
				}
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("owner gate not entered")
			}
			type reply struct {
				status int
				err    error
			}
			result := make(chan reply, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			go func() {
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.api.URL+"/v1/days/2026-10-29", nil)
				req.Header.Set("Authorization", "Bearer "+h.a.AccessToken)
				resp, err := http.DefaultClient.Do(req)
				status := 0
				if resp != nil {
					status = resp.StatusCode
					resp.Body.Close()
				}
				result <- reply{status, err}
			}()
			h.get(h.b, "/v1/days/2026-10-29", http.StatusOK)
			select {
			case got := <-result:
				t.Fatalf("healthy owner queue returned early: status=%d err=%v", got.status, got.err)
			case <-time.After(2300 * time.Millisecond):
			}
			release <- struct{}{}
			holderErr := <-done
			released = true
			if holderErr != nil {
				t.Fatal(holderErr)
			}
			select {
			case got := <-result:
				if got.err != nil || got.status != http.StatusOK {
					t.Fatalf("healthy queue rejected: status=%d err=%v", got.status, got.err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			h.get(h.a, "/v1/days/2026-10-29", http.StatusOK)
		})
	}
}

// 直接走 BFF→Persona 的 HTTP 链路；只接收 TCP 不响应，不需要主备或真实 PG。
func TestPersonaHTTPUnavailableIsBounded(t *testing.T) {
	h := newHarness(t, "memory")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { <-stop; _ = conn.Close() }()
		}
	}()
	defer func() { close(stop); _ = listener.Close(); <-done }()
	h.reader.(*query.Client).BaseURL = "http://" + listener.Addr().String()
	// 故障夹具显式选择短上界；正常客户端保留 30 秒执行预算。
	h.reader.(*query.Client).HTTP = &http.Client{Timeout: 200 * time.Millisecond}
	assertBoundedUnavailable(t, h, "/v1/days/2026-10-29")
}

func assertBoundedUnavailable(t *testing.T, h *harness, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.api.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.a.AccessToken)
	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request did not return HTTP 503 before client deadline: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "1" || time.Since(started) >= 2500*time.Millisecond {
		t.Fatalf("status=%d retry=%q elapsed=%s", resp.StatusCode, resp.Header.Get("Retry-After"), time.Since(started))
	}
}
