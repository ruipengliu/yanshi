package nodesdk

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestBusyGatewayRetryAfter：网关以 503 拒绝握手时，SDK 读出 Retry-After，重连等待不短于它。
func TestBusyGatewayRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := NewClient(Config{URL: "ws" + strings.TrimPrefix(srv.URL, "http"), NodeID: "n1", Token: "t"})
	_, err := c.session(context.Background())
	var busy *busyError
	if !errors.As(err, &busy) || busy.retryAfter != 7*time.Second {
		t.Fatalf("err = %v, want a busyError with retry-after 7s", err)
	}
}
