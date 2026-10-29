package bff_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xinli/bff"
	"xinli/kernel"
)

type streamRequest struct{ token, after string }

func TestStreamResumeAndRevocation(t *testing.T) {
	for _, kind := range []string{"memory", "pg"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, kind)
			h.api.Close()
			var generation atomic.Int64
			generation.Store(1)
			var hold atomic.Bool
			opened := make(chan streamRequest, 8)
			closed := make(chan struct{}, 8)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/stream") {
					r.Header.Set("Authorization", "Bearer kernel-bl")
					h.kernelHTTP(w, r)
					return
				}
				opened <- streamRequest{r.Header.Get("Authorization"), r.URL.Query().Get("after")}
				defer func() { closed <- struct{}{} }()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "id: 7\nevent: event\ndata: {\"seq\":\"7\"}\n\nevent: delta\ndata: {\"snapshot\":true,\"text\":\"draft\"}\n\n")
				w.(http.Flusher).Flush()
				if hold.Load() {
					<-r.Context().Done()
					return
				}
				_, _ = io.WriteString(w, "event: token_expired\ndata: {}\n\n")
			}))
			t.Cleanup(backend.Close)
			client := &kernel.Client{BaseURL: backend.URL, HTTP: &http.Client{Timeout: 30 * time.Second}, Tokens: tokenSource(func(ctx context.Context, line string) (string, error) {
				return fmt.Sprintf("service-%d", generation.Load()), nil
			})}
			h.api = httptest.NewServer(bff.Handler(bff.Options{Accounts: h.accounts, Persona: h.reader, Commands: h.writer, Kernel: client, Tasks: h.tasks, StreamCheckInterval: 10 * time.Millisecond}))
			t.Cleanup(h.api.Close)
			for _, path := range []string{"/v1/threads/s-1/stream?after=-1", "/v1/threads/s-1/stream?after=1&after=2", "/v1/threads/s-1/stream?end_user=other"} {
				h.request(h.a.AccessToken, "GET", path, "", 400)
			}
			open := func(after, last string) (*http.Response, context.CancelFunc) {
				t.Helper()
				ctx, cancel := context.WithCancel(context.Background())
				req, _ := http.NewRequestWithContext(ctx, "GET", h.api.URL+"/v1/threads/s-1/stream?after="+after, nil)
				req.Header.Set("Authorization", "Bearer "+h.a.AccessToken)
				if last != "" {
					req.Header.Set("Last-Event-ID", last)
				}
				resp, err := h.api.Client().Do(req)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
					resp.Body.Close()
					cancel()
					t.Fatalf("stream status %d", resp.StatusCode)
				}
				return resp, cancel
			}
			awaitClosed := func() {
				t.Helper()
				select {
				case <-closed:
				case <-time.After(3 * time.Second):
					t.Fatal("upstream was not closed")
				}
			}
			for i := 1; i <= 2; i++ {
				generation.Store(int64(i))
				last := ""
				wantAfter := "3"
				if i == 2 {
					last, wantAfter = "7", "7"
				}
				resp, cancel := open("3", last)
				data, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				cancel()
				if err != nil || !strings.Contains(string(data), "id: 7\nevent: event") || !strings.Contains(string(data), `"snapshot":true`) || !strings.Contains(string(data), "event: token_expired") {
					t.Fatalf("lost SSE frame: %s, %v", data, err)
				}
				got := <-opened
				if got.after != wantAfter || got.token != fmt.Sprintf("Bearer service-%d", i) {
					t.Fatal("cursor or refreshed token lost")
				}
				awaitClosed()
			}
			hold.Store(true)
			resp, cancel := open("7", "")
			<-opened
			cancel()
			resp.Body.Close()
			awaitClosed()
			resp, cancel = open("7", "")
			<-opened
			defer cancel()
			defer resp.Body.Close()
			ctx, end := context.WithTimeout(context.Background(), 2*time.Second)
			defer end()
			if err := h.accounts.Disable(ctx, h.a.Owner); err != nil {
				t.Fatalf("stream held account gate: %v", err)
			}
			awaitClosed()
			drained := make(chan struct{})
			go func() { _, _ = io.Copy(io.Discard, resp.Body); close(drained) }()
			select {
			case <-drained:
			case <-time.After(3 * time.Second):
				t.Fatal("disabled account stream still open")
			}
			h.request(h.a.AccessToken, "GET", "/v1/threads/s-1/stream", "", 401)
		})
	}
}
