package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDeletionHTTPContract(t *testing.T) {
	ctx := context.Background()
	deadline := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name, method, path, response string
		call                         func(*Client) error
	}{
		{"create", "POST", "/v1/sessions", `{"session_id":"s"}`, func(c *Client) error {
			_, err := c.CreateSessionWithOptions(ctx, "bl", "eu", "xinli-day", SessionOptions{DeleteAt: deadline})
			return err
		}},
		{"session", "GET", "/v1/sessions/s", `{"runs":[{"run_id":"old","status":"completed"},{"run_id":"r","status":"suspended"}]}`, func(c *Client) error {
			state, err := c.Session(ctx, "s")
			if err == nil && state.ActiveRun() != "r" {
				t.Errorf("active run: %+v", state)
			}
			return err
		}},
		{"interrupt", "POST", "/v1/sessions/s/runs/r/interrupt", "", func(c *Client) error { return c.Interrupt(ctx, "s", "r") }},
		{"erase", "DELETE", "/v1/sessions/s?reason=erasure", "", func(c *Client) error { return c.DeleteSession(ctx, "s", "erasure") }},
		{"delete", "DELETE", "/v1/sessions/s", "", func(c *Client) error { return c.DeleteSession(ctx, "s", "") }},
		{"deletion", "GET", "/v1/sessions/s/deletion?business_line=bl", `{"reason":"erasure","requested_at":"2026-10-28T00:00:00Z","completed_at":"2026-10-28T00:01:00Z"}`, func(c *Client) error {
			d, err := c.Deletion(ctx, "bl", "s")
			if err == nil && (d.Reason != "erasure" || d.RequestedAt.IsZero() || d.CompletedAt.IsZero()) {
				t.Errorf("deletion: %+v", d)
			}
			return err
		}},
		{"memories", "GET", "/v1/memories?business_line=bl&end_user=eu&source_session=s", `{"memories":[{"id":"m","content":"候选"}]}`, func(c *Client) error {
			memories, err := c.MemoriesBySource(ctx, "bl", "eu", []string{"s"})
			if err == nil && !reflect.DeepEqual(memories, []Memory{{ID: "m", Content: "候选"}}) {
				t.Errorf("memories: %+v", memories)
			}
			return err
		}},
		{"delete_memories", "DELETE", "/v1/sessions/s/memories?business_line=bl", "", func(c *Client) error { return c.DeleteSessionMemories(ctx, "bl", "s") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, status := range []int{200, 404, 403, 500} {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != tc.method || r.URL.RequestURI() != tc.path {
						t.Errorf("request: %s %s", r.Method, r.URL.RequestURI())
					}
					if r.Header.Get("Authorization") != "Bearer service-token" {
						t.Error("service token missing")
					}
					if tc.name == "create" {
						var body map[string]string
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						if !reflect.DeepEqual(body, map[string]string{"business_line": "bl", "end_user": "eu", "agent": "xinli-day", "delete_at": deadline.Format(time.RFC3339)}) {
							t.Errorf("create: %v", body)
						}
					}
					w.WriteHeader(status)
					if status == 200 {
						_, _ = fmt.Fprint(w, tc.response)
					} else {
						_, _ = fmt.Fprint(w, "private-content")
					}
				}))
				err := tc.call(&Client{BaseURL: server.URL, Token: "service-token"})
				server.Close()
				if status == 200 {
					if err != nil {
						t.Fatal(err)
					}
					continue
				}
				var httpErr *HTTPError
				if !errors.As(err, &httpErr) || httpErr.StatusCode != status || strings.Contains(err.Error(), "private-content") {
					t.Fatalf("error: %v", err)
				}
				if errors.Is(err, ErrNotFound) != (status == 404) {
					t.Fatalf("not found: %v", err)
				}
			}
		})
	}
}

func TestMemoriesBatchAndEmpty(t *testing.T) {
	calls := 0
	var sources []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		batch := r.URL.Query()["source_session"]
		if len(batch) == 0 || len(batch) > 32 {
			t.Errorf("batch size: %d", len(batch))
		}
		sources = append(sources, batch...)
		_, _ = fmt.Fprint(w, `{"memories":[{"id":"m","content":"候选"}]}`)
	}))
	defer server.Close()
	c := &Client{BaseURL: server.URL}
	if got, err := c.MemoriesBySource(context.Background(), "bl", "eu", nil); err != nil || len(got) != 0 || calls != 0 {
		t.Fatal("empty query was sent")
	}
	sessions := make([]string, 65)
	for i := range sessions {
		sessions[i] = fmt.Sprintf("s%d", i)
	}
	memories, err := c.MemoriesBySource(context.Background(), "bl", "eu", sessions)
	if err != nil || calls != 3 || len(memories) != 1 || !reflect.DeepEqual(sources, sessions) {
		t.Fatalf("batch: calls=%d memories=%v err=%v", calls, memories, err)
	}
}

func TestUnrestrictedDeletionBusinessLine(t *testing.T) {
	const bl = "demo & other"
	calls := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- r.Method + " " + r.URL.Path
		if r.Header.Get("Authorization") != "" {
			t.Error("unexpected token")
		}
		if r.URL.Query().Get("business_line") != bl {
			t.Errorf("business line query: %q", r.URL.RawQuery)
		}
		switch r.URL.Path {
		case "/v1/sessions/s/deletion":
			if r.Method != http.MethodGet {
				t.Errorf("deletion method: %s", r.Method)
			}
			_, _ = fmt.Fprint(w, `{"reason":"erasure","requested_at":"2026-10-28T00:00:00Z","completed_at":"2026-10-28T00:01:00Z"}`)
		case "/v1/sessions/s/memories":
			if r.Method != http.MethodDelete {
				t.Errorf("memories method: %s", r.Method)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/v1/memories":
			if r.Method != http.MethodGet || r.URL.Query().Get("end_user") != "eu" || r.URL.Query().Get("source_session") != "s" {
				t.Error("source query")
			}
			_, _ = fmt.Fprint(w, `{"memories":[]}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c := &Client{BaseURL: server.URL}
	ctx := context.Background()
	if _, err := c.Deletion(ctx, bl, "s"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteSessionMemories(ctx, bl, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MemoriesBySource(ctx, bl, "eu", []string{"s"}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatalf("calls: %d", len(calls))
	}
}
