package bff_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"persona/domain"
	"xinli/bff"
	"xinli/kernel"
)

type operationRequest struct {
	Method string
	Path   string
	Query  string
	Body   map[string]json.RawMessage
}

type operationsHarness struct {
	*harness
	lock     sync.Mutex
	calls    []operationRequest
	conflict string
	answered bool
	approved bool
}

func operationsFixture(t *testing.T, kind string) *operationsHarness {
	h := newHarness(t, kind)
	h.api.Close()
	o := &operationsHarness{harness: h}
	backend := httptest.NewServer(http.HandlerFunc(o.serveKernel))
	t.Cleanup(backend.Close)
	client := &kernel.Client{BaseURL: backend.URL, Tokens: tokenSource(func(ctx context.Context, line string) (string, error) { return "kernel-" + line, nil })}
	h.api = httptest.NewServer(bff.Handler(bff.Options{Accounts: h.accounts, Persona: h.reader, Commands: h.writer, Kernel: client, Tasks: h.tasks}))
	t.Cleanup(h.api.Close)
	return o
}

func (o *operationsHarness) serveKernel(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer kernel-bl" {
		w.WriteHeader(403)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/s-1" {
		o.lock.Lock()
		defer o.lock.Unlock()
		runs := []any{}
		if !o.answered {
			runs = append(runs, map[string]any{"run_id": "r-1", "status": "suspended", "waiting": map[string]any{"kind": "question", "call_id": "q-1", "capability": "ask_user"}})
		}
		if !o.approved {
			runs = append(runs, map[string]any{"run_id": "r-2", "status": "suspended", "waiting": map[string]any{"kind": "approval", "call_id": "a-1", "summary": "确认操作"}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"session_id": "s-1", "business_line": "bl", "end_user": o.a.Owner.EndUser, "agent": map[string]string{"name": "xinli-assistant"}, "runs": runs})
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/s-1/events" {
		_ = json.NewEncoder(w).Encode(map[string]any{"events": []any{map[string]any{"session_id": "s-1", "seq": "1", "assistant_message": map[string]any{"tool_calls": []any{map[string]string{"call_id": "q-1", "arguments_json": `{"question":"继续吗？"}`}, map[string]string{"call_id": "a-1", "arguments_json": `{"target":"file"}`}}}}}})
		return
	}
	if r.Method == http.MethodGet && !strings.HasSuffix(r.URL.Path, "/schedules") {
		o.kernelHTTP(w, r)
		return
	}
	var body map[string]json.RawMessage
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	o.lock.Lock()
	defer o.lock.Unlock()
	o.calls = append(o.calls, operationRequest{r.Method, r.URL.Path, r.URL.RawQuery, body})
	if o.conflict != "" {
		w.WriteHeader(409)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": o.conflict})
		return
	}
	switch {
	case r.URL.Path == "/v1/sessions":
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"session_id":"created"}`)
	case strings.HasSuffix(r.URL.Path, "/inputs"):
		w.WriteHeader(202)
		_, _ = io.WriteString(w, `{"run_id":"accepted","duplicate":true}`)
	case strings.HasSuffix(r.URL.Path, "/schedules") && r.Method == http.MethodPost:
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"schedule_id":"timer"}`)
	case strings.HasSuffix(r.URL.Path, "/schedules"):
		_, _ = io.WriteString(w, `{"schedules":[]}`)
	default:
		if strings.Contains(r.URL.Path, "/answers/") {
			o.answered = true
		}
		if strings.Contains(r.URL.Path, "/approvals/") {
			o.approved = true
		}
		w.WriteHeader(204)
	}
}

func (h *harness) request(token, method, path, body string, want int) map[string]json.RawMessage {
	h.t.Helper()
	req, err := http.NewRequest(method, h.api.URL+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.api.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	if resp.StatusCode != want {
		h.t.Fatalf("%s %s: %d want %d: %s", method, path, resp.StatusCode, want, data)
	}
	if strings.Contains(string(data), "private downstream") || strings.Contains(string(data), "kernel-bl") {
		h.t.Fatal("downstream content leaked")
	}
	var out map[string]json.RawMessage
	_ = json.Unmarshal(data, &out)
	return out
}

func TestOperationIsolation(t *testing.T) {
	for _, kind := range []string{"memory", "pg"} {
		t.Run(kind, func(t *testing.T) {
			h := operationsFixture(t, kind)
			operations := []struct{ method, suffix, body string }{
				{"POST", "/inputs", `{"input_id":"i","text":"hello"}`},
				{"POST", "/redo", `{"input_id":"i","expected_run_id":"r"}`},
				{"POST", "/interrupt", `{}`},
				{"DELETE", "", ""},
				{"POST", "/answers/q-1", `{"text":"yes"}`},
				{"POST", "/approvals/a-1", `{"approve":true}`},
				{"POST", "/approvals/a-1", `{"approve":false}`},
				{"POST", "/card-actions", `{"call_id":"card-call","card_id":"card","action":"ok"}`},
				{"POST", "/schedules", `{"schedule_id":"timer","input":"hi","at":"2026-11-01T12:00:00Z","time_zone":"UTC"}`},
				{"GET", "/schedules", ""},
				{"DELETE", "/schedules/timer", ""},
				{"GET", "/questions", ""},
				{"GET", "/approvals", ""},
				{"GET", "/stream", ""},
			}
			for _, id := range []string{"b-thread", "work-day", "work-project", "work-week", "work-brief", "work-new"} {
				for _, op := range operations {
					h.request(h.a.AccessToken, op.method, "/v1/threads/"+id+op.suffix, op.body, 404)
				}
			}
			for _, op := range operations {
				h.request(h.b.AccessToken, op.method, "/v1/threads/s-1"+op.suffix, op.body, 404)
				h.request(h.other.AccessToken, op.method, "/v1/threads/s-1"+op.suffix, op.body, 404)
			}
			h.lock.Lock()
			count := len(h.calls)
			h.lock.Unlock()
			if count != 0 {
				t.Fatal("operation reached kernel before authorization")
			}
			for _, token := range []string{"", "kernel-bl", "channel-credential"} {
				h.request(token, "POST", "/v1/threads/s-1/approvals/a-1", `{"approve":true}`, 401)
			}
		})
	}
}

func TestThreadOperations(t *testing.T) {
	h := operationsFixture(t, "memory")
	token := h.a.AccessToken
	h.request(token, "POST", "/v1/threads", `{"agent":"xinli-day"}`, 400)
	h.request(token, "POST", "/v1/threads", `{}`, 201)
	h.request(token, "POST", "/v1/threads/s-1/inputs", `{"input_id":"client-id","text":"hello"}`, 202)
	h.request(token, "POST", "/v1/threads/s-1/inputs", `{"text":"hello"}`, 400)
	h.request(token, "POST", "/v1/threads/s-1/inputs", `{"input_id":"i","redo":true}`, 400)
	cards := `{"call_id":"card-call","card_id":"card","action":"ok","values":{"date":"2026-11-01"},"seq":"12"}`
	for range 2 {
		out := h.request(token, "POST", "/v1/threads/s-1/card-actions", cards, 202)
		if string(out["duplicate"]) != "true" {
			t.Fatal("lost kernel deduplication result")
		}
	}
	for _, path := range []string{"questions", "approvals"} {
		out := h.get(h.a, "/v1/threads/s-1/"+path, 200)
		if len(items(out)) != 1 || len(items(out)[0]["arguments"]) == 0 {
			t.Fatal("missing pending event parameters")
		}
	}
	h.request(token, "POST", "/v1/threads/s-1/interrupt", `{}`, 204)
	h.request(token, "POST", "/v1/threads/s-1/answers/q-1", `{"selected":["yes"],"values":{"x":"y"}}`, 204)
	h.request(token, "POST", "/v1/threads/s-1/approvals/a-1", `{"approve":false,"by":"forged"}`, 400)
	h.request(token, "POST", "/v1/threads/s-1/approvals/a-1", `{"approve":false}`, 204)
	for _, path := range []string{"questions", "approvals"} {
		if len(items(h.get(h.a, "/v1/threads/s-1/"+path, 200))) != 0 {
			t.Fatal("resolved item still pending")
		}
	}
	h.request(token, "POST", "/v1/threads/s-1/schedules", `{"schedule_id":"timer","input":"hello","rrule":"FREQ=DAILY","time_zone":"UTC"}`, 201)
	h.request(token, "GET", "/v1/threads/s-1/schedules", "", 200)
	h.request(token, "DELETE", "/v1/threads/s-1/schedules/timer", "", 204)
	h.request(token, "DELETE", "/v1/threads/s-1?reason=erasure", "", 400)
	h.request(token, "DELETE", "/v1/threads/s-1", "", 204)
	h.lock.Lock()
	defer h.lock.Unlock()
	if string(h.calls[0].Body["time_zone"]) != `"Asia/Shanghai"` || string(h.calls[0].Body["agent"]) != `"xinli-assistant"` || string(h.calls[0].Body["end_user"]) != fmt.Sprintf("%q", h.a.Owner.EndUser) || string(h.calls[1].Body["input_id"]) != `"client-id"` {
		t.Fatal("creation identity or input ID changed")
	}
	var content []struct {
		Action map[string]json.RawMessage `json:"card_action"`
	}
	if json.Unmarshal(h.calls[2].Body["content"], &content) != nil || len(content) != 1 || string(content[0].Action["call_id"]) != `"card-call"` || string(content[0].Action["seq"]) != `"12"` {
		t.Fatal("card action changed")
	}
}

func TestRedoConflictCategory(t *testing.T) {
	h := operationsFixture(t, "memory")
	for _, reason := range []string{"not_last_run", "not_read_only", "private downstream content"} {
		h.lock.Lock()
		h.conflict = "conflict: redo: " + reason
		h.lock.Unlock()
		out := h.request(h.a.AccessToken, "POST", "/v1/threads/s-1/redo", `{"input_id":"redo-id","expected_run_id":"expected"}`, 409)
		if reason == "private downstream content" {
			if len(out["reason"]) != 0 {
				t.Fatal("unknown reason exposed")
			}
		} else if string(out["reason"]) != fmt.Sprintf("%q", reason) {
			t.Fatal("lost redo category")
		}
	}
	h.lock.Lock()
	defer h.lock.Unlock()
	for _, call := range h.calls {
		if string(call.Body["redo"]) != "true" || string(call.Body["input_id"]) != `"redo-id"` || string(call.Body["expected_run_id"]) != `"expected"` || len(call.Body) != 3 {
			t.Fatal("invalid redo translation")
		}
	}
}

func TestPersonaOperations(t *testing.T) {
	for _, kind := range []string{"memory", "pg"} {
		t.Run(kind, func(t *testing.T) {
			h := operationsFixture(t, kind)
			token := h.a.AccessToken
			out := h.request(token, "PATCH", "/v1/commitments/c-1", `{"rev":2,"fields":{"content":"new","counterpart":"person","due_date":"2026-11-01"}}`, 200)
			if string(out["rev"]) != "3" {
				t.Fatal("revision did not advance")
			}
			h.request(token, "PATCH", "/v1/commitments/c-1", `{"rev":2,"status":"done"}`, 409)
			h.request(token, "PATCH", "/v1/commitments/c-1", `{"rev":3,"status":"done"}`, 200)
			h.request(token, "PATCH", "/v1/commitments/c-3", `{"rev":1,"status":"cancelled"}`, 200)
			h.request(token, "PATCH", "/v1/commitments/private-b", `{"rev":1,"status":"done"}`, 404)
			h.request(token, "PATCH", "/v1/commitments/c-1", `{"rev":4,"fields":{"kind":"theirs"}}`, 400)
			project := h.get(h.a, "/v1/projects/p-1", 200)
			body := fmt.Sprintf(`{"rev":%s,"status":"user status","next_steps":["next"],"open_questions":[]}`, project["rev"])
			h.request(token, "PUT", "/v1/projects/p-1/status", body, 200)
			h.request(token, "PUT", "/v1/projects/p-1/status", body, 409)
			h.request(token, "PUT", "/v1/projects/private-b/status", body, 404)
			if err := h.books.View(h.ctx, h.a.Owner.BusinessLine, h.a.Owner.EndUser, func(book *domain.Book) error {
				st := book.State()
				if st.Changesets[len(st.Changesets)-1].Author != domain.AuthorUser || st.Changesets[len(st.Changesets)-1].ProjectStatus == nil {
					t.Error("missing user authorship")
				}
				m, err := book.QueryCommitment("c-1")
				if err == nil && (len(m.Locked) < 3 || m.Status != "done") {
					t.Error("user field locks lost")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			other := h.get(h.b, "/v1/projects/p-1", 200)
			if string(other["status"]) != `"b status"` {
				t.Fatal("project edit crossed owner")
			}
			deleted := h.request(token, "POST", "/v1/days/2026-10-29/erase", `{"from":"1.0","to":"1.1"}`, 202)
			if string(deleted["status"]) != `"deleting"` {
				t.Fatal("premature deletion completion")
			}
			var id string
			_ = json.Unmarshal(deleted["id"], &id)
			h.request(token, "DELETE", "/v1/deletions/"+id+"/memories/m-1", "", 409)
			page := h.get(h.a, "/v1/deletions", 200)
			if len(items(page)) != 1 || string(items(page)[0]["status"]) != `"deleting"` || len(items(page)[0]["candidates"]) != 0 {
				t.Fatal("pending deletion leaked candidates")
			}
			h.request(h.b.AccessToken, "DELETE", "/v1/deletions/"+id+"/memories/m-1", "", 404)
			if err := h.books.Update(h.ctx, h.a.Owner.BusinessLine, h.a.Owner.EndUser, func(book *domain.Book) error {
				return book.CompleteDeletionTask(id, []domain.MemoryCandidate{{ID: "m-1", Summary: "candidate"}}, h.now)
			}); err != nil {
				t.Fatal(err)
			}
			h.request(token, "DELETE", "/v1/deletions/"+id+"/memories/unknown", "", 404)
			h.lock.Lock()
			before := len(h.calls)
			h.lock.Unlock()
			if before != 0 {
				t.Fatal("unconfirmed candidate reached kernel")
			}
			completed := items(h.get(h.a, "/v1/deletions", 200))
			if len(completed) != 1 || string(completed[0]["status"]) != `"completed"` || len(completed[0]["candidates"]) == 0 {
				t.Fatal("completed candidates unavailable")
			}
			h.request(token, "DELETE", "/v1/deletions/"+id+"/memories/m-1", "", 204)
			h.lock.Lock()
			call := h.calls[len(h.calls)-1]
			h.lock.Unlock()
			if call.Path != "/v1/memories/m-1" || !strings.Contains(call.Query, "end_user="+h.a.Owner.EndUser) || !strings.Contains(call.Query, "business_line=bl") {
				t.Fatal("Memory delete lost owner restriction")
			}
			h.request(token, "DELETE", "/v1/days/2026-10-29", "", 202)
			h.get(h.a, "/v1/days/2026-10-29/captures", 410)
			h.get(h.b, "/v1/days/2026-10-29/captures", 200)
			h.request(token, "DELETE", "/v1/days/2026-10-30", "", 404)
		})
	}
}

func TestAccountOperations(t *testing.T) {
	for _, kind := range []string{"memory", "pg"} {
		t.Run(kind, func(t *testing.T) {
			h := operationsFixture(t, kind)
			h.request(h.a.AccessToken, "PATCH", "/v1/account/preferences", `{"time_zone":"UTC","preferences":{"reminders":false}}`, 204)
			h.get(h.a, "/v1/account/devices", 200)
			record, err := h.tasks.ReadAccount(h.ctx, h.a.Owner)
			if err != nil || record.Account.TimeZone != "UTC" || !strings.Contains(string(record.Account.Preferences), `"reminders"`) {
				t.Fatal("profile was not persisted")
			}
			record, err = h.tasks.ReadAccount(h.ctx, h.b.Owner)
			if err != nil || record.Account.TimeZone == "UTC" {
				t.Fatal("profile crossed owner")
			}
			out := h.request(h.a.AccessToken, "DELETE", "/v1/account", "", 202)
			var receipt string
			_ = json.Unmarshal(out["deletion_token"], &receipt)
			h.request(h.a.AccessToken, "GET", "/v1/account/devices", "", 401)
			h.request(receipt, "GET", "/v1/account/deletion", "", 200)
			h.request(h.b.AccessToken, "GET", "/v1/account/deletion", "", 401)
			h.request(receipt, "POST", "/v1/threads/s-1/approvals/a-1", `{"approve":true}`, 401)
			if err := h.accounts.Disable(h.ctx, h.b.Owner); err != nil {
				t.Fatal(err)
			}
			h.request(h.b.AccessToken, "POST", "/v1/threads", `{}`, 401)
		})
	}
}

func TestResolveProjectOwnership(t *testing.T) {
	for _, kind := range []string{"memory", "pg"} {
		t.Run(kind, func(t *testing.T) {
			h := operationsFixture(t, kind)
			if err := h.books.Update(h.ctx, h.a.Owner.BusinessLine, h.a.Owner.EndUser, func(book *domain.Book) error {
				cites := map[string]domain.Citation{"#1.0": {Ref: "#1.0", Speaker: "wearer"}, "#1.1": {Ref: "#1.1", Speaker: "other"}}
				call := domain.NewCall(book, "unmanaged-fixture", json.RawMessage(`{"source":{"day":true},"exchanges":{"create":[{"from":"#1.0","to":"#1.1","title":"project discussion","projects":[{"match":"unsure","id":"p-1","name":"proposed"}]}]}}`), cites, h.now)
				if result := call.FileChanges(); !strings.HasPrefix(result, "已收下变更集") {
					return fmt.Errorf("fixture rejected: %s", result)
				}
				return call.Err()
			}); err != nil {
				t.Fatal(err)
			}
			day := h.get(h.a, "/v1/days/2026-10-29", 200)
			var exchanges []map[string]json.RawMessage
			_ = json.Unmarshal(day["exchanges"], &exchanges)
			var rev json.RawMessage
			for _, e := range exchanges {
				if string(e["id"]) == `"x1.0"` {
					rev = e["rev"]
				}
			}
			if len(rev) == 0 {
				t.Fatal("pending exchange unavailable to App")
			}
			path := "/v1/days/2026-10-29/exchanges/x1.0/resolve"
			body := fmt.Sprintf(`{"rev":%s,"project":"p-1"}`, rev)
			h.request(h.b.AccessToken, "POST", path, body, 404)
			h.request(h.a.AccessToken, "POST", path, fmt.Sprintf(`{"rev":%s,"project":"private-b"}`, rev), 404)
			h.request(h.a.AccessToken, "POST", path, body, 200)
			h.request(h.a.AccessToken, "POST", path, body, 409)
		})
	}
}
