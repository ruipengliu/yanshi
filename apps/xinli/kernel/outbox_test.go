package kernel_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"xinli/kernel"
)

func TestRecoveryHTTPContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("business_line") != "bl" {
			t.Error("missing business line")
		}
		switch r.URL.Path {
		case "/v1/outbox/shards":
			if q.Has("after") || q.Has("kinds") {
				t.Error("head must use the same all-kinds filter without after")
			}
			fmt.Fprint(w, `{"shards":[{"shard":0,"status":"ready","next":"H"},{"shard":2,"status":"ready","next":"H2"}]}`)
		case "/v1/outbox":
			if q.Get("shard") != "2" || q.Get("after") != "H2" || q.Get("limit") != "10" || q.Has("kinds") {
				t.Errorf("outbox parameters: %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"events":[{"envelope_id":"e","business_line":"bl","end_user":"u","session_id":"s","seq":2,"kind":"question"}],"next":"N"}`)
		case "/v1/sessions":
			if q.Get("order") != "session_id" || q.Get("end_user") != "u" || q.Get("cursor") != "a" || q.Get("limit") != "10" {
				t.Errorf("stable parameters: %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"sessions":[{"session_id":"b","end_user":"u","closed_at":"2026-01-01T00:00:00Z"}],"next_cursor":""}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	c := &kernel.Client{BaseURL: server.URL}
	ctx := context.Background()
	heads, err := c.OutboxHeads(ctx, "bl")
	if err != nil || len(heads) != 2 || heads[1].Next != "H2" {
		t.Fatalf("heads: %+v %v", heads, err)
	}
	page, err := c.Outbox(ctx, "bl", 2, "H2", 10)
	if err != nil || page.Next != "N" || len(page.Events) != 1 {
		t.Fatalf("outbox: %+v %v", page, err)
	}
	rows, err := c.StableSessions(ctx, "bl", "u", "a", 10)
	if err != nil || len(rows) != 1 || rows[0].ID != "b" {
		t.Fatalf("sessions: %+v %v", rows, err)
	}
}

func TestOnlyExplicitOutbox410StartsRecovery(t *testing.T) {
	for _, tc := range []struct {
		status int
		reason string
		reset  bool
	}{
		{410, "outbox_cursor_expired", true}, {410, "outbox_epoch_changed", true},
		{410, "unknown", false}, {503, "outbox_cursor_expired", false},
		{409, "outbox_filter_changed", false}, {403, "outbox_cursor_scope_mismatch", false},
	} {
		t.Run(tc.reason+fmt.Sprint(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"error":%q}`, tc.reason)
			}))
			defer server.Close()
			c := &kernel.Client{BaseURL: server.URL}
			_, err := c.Outbox(context.Background(), "bl", 0, "cursor", 10)
			if err == nil || kernel.OutboxResetRequired(err) != tc.reset {
				t.Fatalf("reset=%v err=%v", kernel.OutboxResetRequired(err), err)
			}
		})
	}
}
