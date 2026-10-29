package kernel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamUsesRefreshedTokenAndIndependentTimeout(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var unix atomic.Int64
	unix.Store(now.Unix())
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "key.pem")
	writeKey(t, path, "bl", "kid", key)
	source, err := NewTokenSource(map[string]TokenLocation{"bl": {KeyFile: path}}, TokenOptions{Now: func() time.Time { return time.Unix(unix.Load(), 0) }})
	if err != nil {
		t.Fatal(err)
	}
	tokens := make(chan string, 2)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens <- r.Header.Get("Authorization")
		if r.URL.Query().Get("after") != "9" {
			t.Error("lost resume sequence")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "event: token_expired\ndata: {}\n\n")
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Tokens: source, HTTP: &http.Client{Timeout: time.Millisecond}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first, err := client.Stream(ctx, "bl", "s", 9)
	if err != nil {
		t.Fatal(err)
	}
	firstToken := <-tokens
	// 已打开的流跨过普通 HTTP Client 的总超时，仍等待上游的令牌到期帧。
	read := make(chan error, 1)
	go func() {
		data, err := io.ReadAll(first)
		if err == nil && !strings.Contains(string(data), "token_expired") {
			err = io.ErrUnexpectedEOF
		}
		read <- err
	}()
	select {
	case err := <-read:
		first.Close()
		t.Fatalf("stream ended before upstream: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-read; err != nil {
		t.Fatal(err)
	}
	first.Close()
	unix.Store(now.Add(14 * time.Minute).Unix())
	second, err := client.Stream(ctx, "bl", "s", 9)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
	if refreshed := <-tokens; refreshed == firstToken || refreshed == "" {
		t.Fatal("stream reconnect reused expired service token")
	}
}
