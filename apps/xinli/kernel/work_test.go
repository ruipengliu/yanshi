package kernel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestReceiptUnsafeWrites(t *testing.T) {
	for _, mode := range []string{"ok", "readonly_error", "write_error", "missing_result", "takeover", "clean_resume", "pending_resume", "confirmed_resume"} {
		t.Run(mode, func(t *testing.T) {
			events := []map[string]any{{"run_requested": map[string]any{"run_id": "r", "input_id": "i"}}}
			attempt := 1
			if mode == "takeover" || mode == "clean_resume" || mode == "pending_resume" || mode == "confirmed_resume" {
				attempt = 2
			}
			if mode == "clean_resume" || mode == "pending_resume" || mode == "confirmed_resume" {
				events = append(events, map[string]any{"attempt_started": map[string]any{"run_id": "r", "attempt": 1}})
				if mode == "pending_resume" || mode == "confirmed_resume" {
					events = append(events, map[string]any{"tool_call_started": map[string]any{"run_id": "r", "call_id": "pending"}})
				}
				events = append(events, map[string]any{"run_suspended": map[string]any{"run_id": "r", "reason": "handoff"}})
				if mode == "confirmed_resume" {
					events = append(events, map[string]any{"tool_result": map[string]any{"run_id": "r", "call_id": "pending"}})
				}
			}
			events = append(events, map[string]any{"attempt_started": map[string]any{"run_id": "r", "attempt": attempt}}, map[string]any{"tool_call_started": map[string]any{"run_id": "r", "call_id": "c", "read_only": mode == "readonly_error"}})
			if mode != "missing_result" {
				events = append(events, map[string]any{"tool_result": map[string]any{"run_id": "r", "call_id": "c", "is_error": mode == "write_error" || mode == "readonly_error"}})
			}
			if mode == "pending_resume" {
				events = append(events, map[string]any{"tool_result": map[string]any{"run_id": "r", "call_id": "pending"}})
			}
			events = append(events, map[string]any{"run_completed": map[string]any{"run_id": "r"}})
			for i, e := range events {
				e["seq"] = strconv.Itoa(i + 1)
				e["session_id"] = "s"
				e["id"] = "event" + strconv.Itoa(i+1)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				after, _ := strconv.Atoi(r.URL.Query().Get("after"))
				page := []map[string]any{}
				if after < len(events) {
					page = events[after : after+1]
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"events": page})
			}))
			defer server.Close()
			c := &Client{BaseURL: server.URL}
			receipt, found, err := c.FindInput(context.Background(), "s", "i")
			want := mode != "ok" && mode != "readonly_error" && mode != "clean_resume" && mode != "confirmed_resume"
			if err != nil || !found || receipt.Terminal != "completed" || receipt.Unsafe != want {
				t.Fatal(receipt, found, err)
			}
		})
	}
}

func TestOrphanPublicQueries(t *testing.T) {
	for _, mode := range []string{"empty", "content", "invalid_cursor"} {
		t.Run(mode, func(t *testing.T) {
			pages := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/sessions" {
					pages++
					if r.URL.Query().Get("business_line") != "bl" || r.URL.Query().Get("end_user") != "u" {
						t.Error("missing owner")
					}
					next := ""
					if pages == 1 || mode == "invalid_cursor" {
						next = "cursor"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]any{{"session_id": "s", "end_user": "u", "agent": "xinli-day", "created_at": "2026-10-28T12:00:00Z"}}, "next_cursor": next})
					return
				}
				events := []map[string]any{{"session_id": "s", "seq": "1", "session_created": map[string]any{"business_line": "bl"}}}
				if mode == "content" {
					events = append(events, map[string]any{"session_id": "s", "seq": "2", "run_requested": map[string]any{"run_id": "r"}})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"events": events})
			}))
			defer server.Close()
			c := &Client{BaseURL: server.URL}
			list, err := c.Sessions(context.Background(), "bl", "u")
			if mode == "invalid_cursor" {
				if err == nil {
					t.Fatal("cursor loop accepted")
				}
				return
			}
			if err != nil || len(list) != 2 || pages != 2 {
				t.Fatal(list, err, pages)
			}
			empty, err := c.EmptySession(context.Background(), "s")
			if err != nil || empty != (mode == "empty") {
				t.Fatal(empty, err)
			}
		})
	}
}
