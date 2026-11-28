package kernel

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

type inputStatusTransport struct{ handler http.Handler }

func (tr inputStatusTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	tr.handler.ServeHTTP(w, r)
	return w.Result(), nil
}

func inputStatusTestClient(handler http.Handler) Client {
	return Client{BaseURL: "http://kernel", HTTP: &http.Client{Transport: inputStatusTransport{handler}}}
}

func TestInputStatusReceiptMatchesReplay(t *testing.T) {
	call := func(id string, readOnly bool) map[string]any {
		return map[string]any{"call_id": id, "read_only": readOnly}
	}
	result := func(id string, isError bool) map[string]any {
		return map[string]any{"call_id": id, "is_error": isError}
	}
	type fields = map[string]any
	type entry struct {
		kind string
		body fields
	}
	for _, tc := range []struct {
		name      string
		before    []entry
		kind      string
		after     []entry
		calls     inputCallEvidence
		takeovers uint
		suffix    *fields
		unsafe    bool
	}{
		{name: "readonly_error", kind: "requested", after: []entry{
			{"attempt_started", fields{"attempt": 1}}, {"tool_call_started", call("c", true)},
			{"tool_result", result("c", true)},
		}},
		{name: "write_error", kind: "requested", after: []entry{
			{"attempt_started", fields{"attempt": 1}}, {"tool_call_started", call("c", false)},
			{"tool_result", result("c", true)},
		}, calls: inputCallEvidence{WritesWithError: 1}, unsafe: true},
		{name: "pending_at_terminal", kind: "requested", after: []entry{
			{"attempt_started", fields{"attempt": 1}}, {"tool_call_started", call("c", false)},
		}, calls: inputCallEvidence{WritesWithoutResult: 1}, unsafe: true},
		{name: "takeover", kind: "requested", after: []entry{
			{"attempt_started", fields{"attempt": 1}}, {"attempt_started", fields{"attempt": 2}},
		}, takeovers: 1, unsafe: true},
		{name: "pending_at_resume", kind: "requested", after: []entry{
			{"attempt_started", fields{"attempt": 1}}, {"tool_call_started", call("c", false)},
			{"run_suspended", fields{}}, {"attempt_started", fields{"attempt": 2}}, {"tool_result", result("c", false)},
		}, calls: inputCallEvidence{ResumesWithPendingWrites: 1}, unsafe: true},
		{name: "confirmed_before_resume", kind: "requested", after: []entry{
			{"attempt_started", fields{"attempt": 1}}, {"tool_call_started", call("c", false)},
			{"run_suspended", fields{}}, {"tool_result", result("c", false)}, {"attempt_started", fields{"attempt": 2}},
		}},
		{name: "steered_after_error_and_takeover", kind: "steered", before: []entry{
			{"run_requested", fields{"input_id": "first"}}, {"attempt_started", fields{"attempt": 1}},
			{"attempt_started", fields{"attempt": 2}}, {"tool_call_started", call("c", false)}, {"tool_result", result("c", true)},
		}, calls: inputCallEvidence{WritesWithError: 1}, takeovers: 1, suffix: &fields{"calls": inputCallEvidence{}}},
		{name: "steered_during_suspend", kind: "steered", before: []entry{
			{"run_requested", fields{"input_id": "first"}}, {"attempt_started", fields{"attempt": 1}}, {"run_suspended", fields{}},
		}, after: []entry{{"attempt_started", fields{"attempt": 2}}},
			suffix: &fields{"attempts_without_prior_suspend": 1, "calls": inputCallEvidence{}}, unsafe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acceptance := "run_requested"
			if tc.kind == "steered" {
				acceptance = "steered"
			}
			log := append(tc.before, entry{acceptance, fields{"input_id": "i & 1"}})
			log = append(log, tc.after...)
			terminalEvent, terminalStatus := "run_completed", "completed"
			if tc.name == "pending_at_terminal" {
				terminalEvent, terminalStatus = "run_failed", "failed"
			}
			log = append(log, entry{terminalEvent, fields{}})
			events := make([]fields, 0, len(log))
			for i, e := range log {
				e.body["run_id"] = "r"
				events = append(events, fields{"id": fmt.Sprint(i + 1), "seq": fmt.Sprint(i + 1), "session_id": "s", e.kind: e.body})
			}
			seq := fmt.Sprint(len(tc.before) + 1)
			status := fields{"found": true, "as_of_seq": fmt.Sprint(len(log)), "accepted_seq": seq, "run_status": terminalStatus,
				"input_id": "i & 1", "event_id": seq, "run_id": "r", "acceptance_kind": tc.kind, "takeover_count": tc.takeovers, "calls": tc.calls}
			if tc.suffix != nil {
				status["since_acceptance"] = tc.suffix
			}
			for _, mode := range []string{"current", "old_404", "old_405", "over_budget"} {
				mux := http.NewServeMux()
				eventPages := 0
				mux.HandleFunc("GET /v1/sessions/s/events", func(w http.ResponseWriter, r *http.Request) {
					if mode == "current" {
						t.Fatal("new kernel fetched Events")
					}
					eventPages++
					after, _ := strconv.Atoi(r.URL.Query().Get("after"))
					page := events[after:min(after+1, len(events))]
					_ = json.NewEncoder(w).Encode(fields{"events": page})
				})
				switch mode {
				case "current":
					mux.HandleFunc("GET /v1/sessions/s/inputs", func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Query().Get("input_id") != "i & 1" {
							t.Fatal("incorrect input_id encoding")
						}
						_ = json.NewEncoder(w).Encode(status)
					})
				case "old_405":
					mux.HandleFunc("POST /v1/sessions/s/inputs", func(http.ResponseWriter, *http.Request) {})
				case "over_budget":
					mux.HandleFunc("GET /v1/sessions/s/inputs", func(w http.ResponseWriter, _ *http.Request) {
						w.WriteHeader(http.StatusUnprocessableEntity)
						_, _ = fmt.Fprint(w, `{"code":"scan_budget_exceeded"}`)
					})
				}
				c := inputStatusTestClient(mux)
				got, found, err := c.FindInput(t.Context(), "s", "i & 1")
				want := InputReceipt{InputID: "i & 1", RunID: "r", EventID: seq, Sequence: uint64(len(tc.before) + 1),
					Steered: tc.kind == "steered", Terminal: terminalStatus, Unsafe: tc.unsafe}
				if err != nil || !found || got != want {
					t.Fatalf("%s got %+v, %t, %v; want %+v", mode, got, found, err, want)
				}
				if mode == "over_budget" && eventPages != len(events)+1 {
					t.Fatalf("%s read %d pages, want %d", mode, eventPages, len(events)+1)
				}
			}
		})
	}
}

func TestInputStatusAnswerAndFailures(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		code   string
		reason string
		found  bool
	}{
		{http.StatusOK, `{"found":false,"as_of_seq":"1"}`, "", "", false},
		{http.StatusOK, `{"found":true,"as_of_seq":"5","input_id":"i","run_id":"r","event_id":"e","accepted_seq":"4","acceptance_kind":"answered","answered_call_id":"q","run_status":"suspended","calls":{}}`, "", "", true},
		{http.StatusNotFound, `{"code":"not_found"}`, "not_found", "", false},
		{http.StatusConflict, `{"code":"conflict","reason":"input_accepted_more_than_once"}`, "conflict", "input_accepted_more_than_once", false},
	} {
		c := inputStatusTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/sessions/s/inputs" {
				t.Fatal("unexpected fallback")
			}
			w.WriteHeader(tc.status)
			_, _ = fmt.Fprint(w, tc.body)
		}))
		got, found, err := c.FindInput(t.Context(), "s", "i")
		if tc.code != "" {
			var e *HTTPError
			if found || !errors.As(err, &e) || e.Code != tc.code || e.Reason != tc.reason || e.StatusCode != tc.status {
				t.Fatalf("status %d: %+v, %t, %v", tc.status, got, found, err)
			}
		} else {
			var want InputReceipt
			if tc.found {
				want = InputReceipt{InputID: "i", RunID: "r", EventID: "e", Sequence: 4, Steered: true, Unsafe: true}
			}
			if err != nil || found != tc.found || got != want {
				t.Fatalf("got %+v, %t, %v; want %+v", got, found, err, want)
			}
		}
	}
}
