package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/lifecycle"
	"yanshi/internal/model"
)

func TestInputStatusHTTP(t *testing.T) {
	env := newAuthzFixture(t)
	s := env.api.Service
	ctx := t.Context()
	res, err := s.SubmitWithID(ctx, env.sid, "lookup", model.TextBlocks("插话"))
	if err != nil {
		t.Fatal(err)
	}
	api := env.api.Handler()
	base := "/v1/sessions/" + env.sid
	get := func(path, principal string, want int) map[string]any {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		req.Header.Set("Authorization", "Bearer "+env.token(t, principal, time.Hour))
		w := httptest.NewRecorder()
		api.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s = %d, want %d: %s", principal, path, w.Code, want, w.Body.String())
		}
		if want == http.StatusUnprocessableEntity && len(w.Header().Values("Retry-After")) != 0 {
			t.Fatalf("over budget returned Retry-After: %v", w.Header().Values("Retry-After"))
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	for _, principal := range []string{"demo/u1", "demo/", "demo/u2", "other/u1", "other/"} {
		want := http.StatusNotFound
		if principal == "demo/u1" || principal == "demo/" {
			want = http.StatusOK
		}
		get("/events", principal, want)
		body := get("/inputs?input_id=lookup", principal, want)
		if want == http.StatusOK {
			if _, ok := body["as_of_seq"].(string); !ok {
				t.Fatalf("as_of_seq is not a decimal string: %v", body)
			}
			if _, ok := body["accepted_seq"].(string); !ok {
				t.Fatalf("accepted_seq is not a decimal string: %v", body)
			}
			if body["found"] != true || body["input_id"] != "lookup" || body["run_id"] != res.RunID ||
				body["event_id"] == "" || body["acceptance_kind"] != "steered" || body["calls"] == nil {
				t.Fatalf("hit: %v", body)
			}
		} else if body["code"] != "not_found" {
			t.Fatalf("lookup must use structured 404: %v", body)
		}
	}
	if got := get("/inputs?input_id=missing", "demo/", http.StatusOK); len(got) != 2 || got["found"] != false || got["as_of_seq"] == nil {
		t.Fatalf("miss: %v", got)
	}
	if got := get("/inputs", "demo/", http.StatusBadRequest); got["code"] != "invalid_argument" {
		t.Fatalf("invalid: %v", got)
	}
	st, err := s.Load(ctx, env.sid)
	if err != nil {
		t.Fatal(err)
	}
	addSteered := func(count int, repeatedID string) {
		t.Helper()
		events := make([]*v1.Event, 0, count+1)
		for i := range count {
			id := ""
			if repeatedID != "" {
				id = fmt.Sprint(i)
			}
			events = append(events, &v1.Event{Payload: &v1.Event_Steered{Steered: &v1.Steered{RunId: res.RunID, InputId: id}}})
		}
		if repeatedID != "" {
			events = append(events, &v1.Event{Payload: &v1.Event_Steered{Steered: &v1.Steered{RunId: res.RunID, InputId: repeatedID}}})
		}
		if err := s.Store.Commit(ctx, st, events...); err != nil {
			t.Fatal(err)
		}
	}
	addSteered(65, "lookup")
	if got := get("/inputs?input_id=lookup", "demo/", http.StatusConflict); got["code"] != "conflict" || got["reason"] != "input_accepted_more_than_once" {
		t.Fatalf("conflict: %v", got)
	}
	addSteered(4096-int(st.Seq), "")
	get("/inputs?input_id=missing", "demo/", http.StatusOK)
	addSteered(1, "")
	if got := get("/inputs?input_id=lookup", "demo/", http.StatusUnprocessableEntity); got["code"] != "scan_budget_exceeded" {
		t.Fatalf("budget: %v", got)
	}
	if _, err := s.Deletions.Mark(ctx, &lifecycle.Tombstone{
		SessionID: env.sid, BusinessLine: "demo", RequestedAt: s.Store.Clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if got := get("/inputs?input_id=lookup", "demo/", http.StatusNotFound); got["code"] != "not_found" {
		t.Fatalf("deleted: %v", got)
	}
}
