package bff_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"persona/command"
	"persona/domain"
	"persona/mcpserver"
	personaTest "persona/pg/pgtest"
	"persona/query"
	"persona/store"
	personaMem "persona/store/memstore"
	personaPG "persona/store/pgstore"
	"xinli/account"
	"xinli/bff"
	"xinli/delivery"
	"xinli/kernel"
	appTest "xinli/pg/pgtest"
	"xinli/task"
	taskMem "xinli/task/memstore"
	taskPG "xinli/task/pgstore"
)

type appStore interface {
	account.Store
	task.Store
	task.Reader
}

type tokenSource func(context.Context, string) (string, error)

func (f tokenSource) Token(ctx context.Context, line string) (string, error) { return f(ctx, line) }

type thread struct{ id, user, agent string }

type harness struct {
	t                  *testing.T
	ctx                context.Context
	now                time.Time
	clock              atomic.Int64
	mu                 sync.Mutex
	books              store.Store
	reader             query.Reader
	writer             command.Writer
	tasks              appStore
	accounts           *account.Service
	a, b, other        account.Credentials
	api                *httptest.Server
	threads            []thread
	eventReads         int
	kernelStatus       int
	deleteDuringEvents bool
}

func newHarness(t *testing.T, kind string) *harness {
	t.Helper()
	h := &harness{t: t, ctx: context.Background(), now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	h.clock.Store(h.now.UnixNano())
	clock := func() time.Time { return time.Unix(0, h.clock.Load()).UTC() }
	if kind == "pg" {
		h.books = personaPG.New(personaTest.New(t), clock)
		h.tasks = taskPG.New(appTest.New(t))
	} else {
		h.books = personaMem.New(clock)
		h.tasks = taskMem.New()
	}
	h.accounts = &account.Service{Store: h.tasks, Now: clock, AccessTTL: 30 * 24 * time.Hour, RefreshTTL: 31 * 24 * time.Hour,
		Provider: account.TestProvider{Identities: []account.TestIdentity{
			{BusinessLine: "bl", Subject: "a", Code: "proof-a"},
			{BusinessLine: "bl", Subject: "b", Code: "proof-b"},
			{BusinessLine: "other", Subject: "a", Code: "proof-a"},
		}}}
	login := func(line, proof string) account.Credentials {
		credentials, err := h.accounts.Login(h.ctx, line, proof, "phone", "test")
		if err != nil {
			t.Fatal(err)
		}
		return credentials
	}
	h.a, h.b, h.other = login("bl", "proof-a"), login("bl", "proof-b"), login("other", "proof-a")
	for i, credentials := range []account.Credentials{h.a, h.b, h.other} {
		owner := credentials.Owner
		label := []string{"a", "b", "other"}[i]
		err := h.books.Update(h.ctx, owner.BusinessLine, owner.EndUser, func(book *domain.Book) error {
			book.Seed(domain.SeedData{
				Projects:    []domain.Project{{ID: "p-1", Name: label + " project", Status: label + " status", NextSteps: []string{"next"}, OpenQuestions: []string{"question"}}, {ID: "p-2", Name: "second"}, {ID: "private-" + label, Name: label + " only"}},
				Filed:       []domain.Day{{Date: "2026-10-29", Headline: label + " headline"}},
				Exchanges:   []domain.SeedExchange{{ID: "x1", Date: "2026-10-29", Project: "p-1", Title: label + " exchange"}},
				Commitments: []domain.SeedCommitment{{ID: "c-1", Kind: "mine", Content: label + " commitment", Project: "p-1", DueDate: "2026-10-30", Revisions: []domain.SeedRevision{{Fields: map[string]string{"due_date": "2026-10-31"}}}}, {ID: "c-2", Kind: "mine", Content: "done", Status: "done", Project: "p-2"}, {ID: "c-3", Kind: "theirs", Content: "later", Project: "p-1", DueDate: "2026-10-31"}, {ID: "private-" + label, Content: label + " private", Kind: "mine"}},
			}, h.now)
			_, err := book.AppendCaptures("2026-10-29", "capture", []domain.CaptureSegment{{Speaker: "wearer", Text: label + " first", SpokenAt: "2026-10-29T12:00:00Z", TimeZone: "UTC"}, {Speaker: "other", SpeakerKey: "speaker", SpeakerRef: "person-1", Text: label + " second", SpokenAt: "2026-10-29T12:00:01Z", TimeZone: "UTC"}, {Speaker: "wearer", Text: label + " third", SpokenAt: "2026-10-29T12:00:02Z", TimeZone: "UTC"}})
			if err != nil {
				return err
			}
			book.BindWeek("2026-10-26", "week-"+label)
			weekInput, _ := json.Marshal(map[string][]string{"suggestions": {label + " weekly"}})
			call := domain.NewCall(book, "week-"+label, weekInput, nil, h.now)
			_ = call.FileWeek()
			return call.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	h.threads = []thread{{"s-3", h.a.Owner.EndUser, "xinli-assistant"}, {"work-day", h.a.Owner.EndUser, "xinli-day"}, {"s-1", h.a.Owner.EndUser, "xinli-assistant"}, {"work-project", h.a.Owner.EndUser, "xinli-project"}, {"work-week", h.a.Owner.EndUser, "xinli-week"}, {"work-brief", h.a.Owner.EndUser, "xinli-brief"}, {"work-new", h.a.Owner.EndUser, "future-internal"}, {"b-thread", h.b.Owner.EndUser, "xinli-assistant"}}
	backend := httptest.NewServer(http.HandlerFunc(h.kernelHTTP))
	t.Cleanup(backend.Close)
	service := &query.Service{Store: h.books, Now: clock}
	credentials := []mcpserver.Credential{{BusinessLine: "bl", Token: "persona-bl", ExpiresAt: h.now.AddDate(1, 0, 0)}, {BusinessLine: "other", Token: "persona-other", ExpiresAt: h.now.AddDate(1, 0, 0)}}
	personaMux := http.NewServeMux()
	personaMux.Handle("/v1/query/", query.Handler(service, credentials, clock))
	personaMux.Handle("/v1/commands/", command.Handler(&command.Service{Store: h.books, Now: clock}, credentials, clock))
	persona := httptest.NewServer(personaMux)
	t.Cleanup(persona.Close)
	reader := &query.Client{BaseURL: persona.URL, Token: func(ctx context.Context, line string) (string, error) { return "persona-" + line, nil }}
	h.reader = reader
	h.writer = &command.Client{BaseURL: persona.URL, Token: reader.Token}
	client := &kernel.Client{BaseURL: backend.URL, Tokens: tokenSource(func(ctx context.Context, line string) (string, error) { return "kernel-" + line, nil })}
	h.api = httptest.NewServer(bff.Handler(bff.Options{Accounts: h.accounts, Persona: reader, Commands: h.writer, Kernel: client, Tasks: h.tasks, Inbox: h.tasks.(delivery.Store), Now: clock}))
	t.Cleanup(h.api.Close)
	return h
}

func (h *harness) kernelHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer kernel-bl" {
		w.WriteHeader(403)
		return
	}
	if h.kernelStatus != 0 {
		w.WriteHeader(h.kernelStatus)
		_, _ = io.WriteString(w, "private downstream error")
		return
	}
	respond := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	if r.URL.Path == "/v1/sessions" {
		if r.URL.Query().Get("business_line") != "bl" || r.URL.Query().Get("end_user") == "" {
			h.t.Error("missing owner")
		}
		items := []kernel.SessionInfo{}
		for _, item := range h.threads {
			if item.user == r.URL.Query().Get("end_user") {
				items = append(items, kernel.SessionInfo{ID: item.id, EndUser: item.user, Agent: item.agent, CreatedAt: time.Unix(0, h.clock.Load()).UTC()})
			}
		}
		// 强制内核也翻页，验证 BFF 不把一页内部 Session 当作列表结束。
		start, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
		end := min(start+2, len(items))
		next := ""
		if end < len(items) {
			next = strconv.Itoa(end)
		}
		respond(map[string]any{"sessions": items[start:end], "next_cursor": next})
		return
	}
	if r.URL.Path == "/v1/quota" {
		user := r.URL.Query().Get("end_user")
		if r.URL.Query().Get("business_line") != "bl" || user == "" {
			h.t.Error("quota owner")
		}
		respond(map[string]any{"periods": []kernel.QuotaPeriod{{Scope: "business_line", BusinessLine: "bl", SpentMicros: 999999}, {Scope: "end_user", BusinessLine: "bl", EndUser: user, Tier: "plus", LimitMicros: 1000, SpentMicros: 10}}})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/sessions/"), "/")
	for _, item := range h.threads {
		if parts[0] != item.id {
			continue
		}
		if len(parts) == 1 {
			respond(map[string]any{"session_id": item.id, "business_line": "bl", "end_user": item.user, "agent": map[string]string{"name": item.agent}})
			return
		}
		if len(parts) == 2 && parts[1] == "events" {
			h.eventReads++
			after, _ := strconv.Atoi(r.URL.Query().Get("after"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			events := []map[string]any{}
			for seq := after + 1; seq <= min(3, after+limit); seq++ {
				events = append(events, map[string]any{"session_id": item.id, "seq": strconv.Itoa(seq), "id": fmt.Sprint(seq), "text": "answer"})
			}
			respond(map[string]any{"events": events})
			if h.deleteDuringEvents {
				h.threads = slices.DeleteFunc(h.threads, func(t thread) bool { return t.id == item.id })
			}
			return
		}
	}
	w.WriteHeader(404)
}

func (h *harness) get(credentials account.Credentials, path string, want int) map[string]json.RawMessage {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.api.URL+path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
	resp, err := h.api.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	if resp.StatusCode != want {
		h.t.Fatalf("%s: status %d want %d: %s", path, resp.StatusCode, want, body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		h.t.Fatal("missing no-store")
	}
	if strings.Contains(string(body), "private downstream") || strings.Contains(string(body), "kernel-bl") || strings.Contains(string(body), "persona-bl") {
		h.t.Fatal("leaked downstream data")
	}
	var out map[string]json.RawMessage
	_ = json.Unmarshal(body, &out)
	return out
}

func next(page map[string]json.RawMessage) string {
	var cursor string
	_ = json.Unmarshal(page["next_cursor"], &cursor)
	return url.QueryEscape(cursor)
}
func items(page map[string]json.RawMessage) []map[string]json.RawMessage {
	var result []map[string]json.RawMessage
	_ = json.Unmarshal(page["items"], &result)
	return result
}

func TestQueries(t *testing.T) {
	for _, kind := range []string{"memory", "pg"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, kind)
			for _, path := range []string{"/v1/today", "/v1/days/2026-10-29", "/v1/weeks/2026-10-26", "/v1/projects/p-1", "/v1/projects/p-1/exchanges", "/v1/commitments", "/v1/commitments/c-1/history", "/v1/days/2026-10-29/captures"} {
				page := h.get(h.a, path, 200)
				raw, _ := json.Marshal(page)
				for _, secret := range []string{"b project", "b headline", "b commitment", "b exchange", "b first", "b weekly", "other project", "other headline", "other commitment", "other exchange", "other first", "other weekly"} {
					if strings.Contains(string(raw), secret) {
						t.Fatal("owner leaked")
					}
				}
			}
			day := h.get(h.a, "/v1/today", 200)
			if string(day["capture_revision"]) != "1" || string(day["processed"]) != "false" || string(day["processing_position"]) != `"0"` {
				t.Fatal("today processing state")
			}
			for _, path := range []string{"/v1/projects/private-b", "/v1/commitments/private-b/history", "/v1/projects/private-other", "/v1/threads/b-thread/events", "/v1/threads/work-day/events", "/v1/threads/work-project/events", "/v1/threads/work-week/events", "/v1/threads/work-new/events", "/v1/notes"} {
				h.get(h.a, path, 404)
			}
			h.mu.Lock()
			reads := h.eventReads
			h.mu.Unlock()
			if reads != 0 {
				t.Fatal("read events before checking owner and purpose")
			}
			page := h.get(h.a, "/v1/projects?limit=1", 200)
			if len(items(page)) != 1 || next(page) == "" {
				t.Fatal("project pagination")
			}
			h.get(h.b, "/v1/projects?limit=1&cursor="+next(page), 400)
			h.get(h.other, "/v1/projects?limit=1&cursor="+next(page), 400)
			page = h.get(h.a, "/v1/projects?limit=1&cursor="+next(page), 200)
			if string(items(page)[0]["id"]) != `"p-2"` {
				t.Fatal("project order")
			}
			page = h.get(h.a, "/v1/commitments?status=open&project=p-1&due_from=2026-10-31&due_to=2026-10-31&limit=1", 200)
			if string(items(page)[0]["id"]) != `"c-1"` {
				t.Fatal("commitment filter/order")
			}
			page = h.get(h.a, "/v1/commitments?status=open&project=p-1&due_from=2026-10-31&due_to=2026-10-31&limit=1&cursor="+next(page), 200)
			if string(items(page)[0]["id"]) != `"c-3"` {
				t.Fatal("commitment second page")
			}
			page = h.get(h.a, "/v1/commitments/c-1/history?limit=1", 200)
			if !strings.Contains(string(items(page)[0]["fields"]), "2026-10-30") {
				t.Fatal("creation must retain original due date")
			}
			page = h.get(h.a, "/v1/commitments/c-1/history?limit=1&cursor="+next(page), 200)
			if !strings.Contains(string(items(page)[0]["fields"]), "2026-10-31") {
				t.Fatal("revision missing")
			}
			page = h.get(h.a, "/v1/days/2026-10-29/captures?limit=1", 200)
			if string(items(page)[0]["text"]) != `"a first"` {
				t.Fatal("capture first page")
			}
			page = h.get(h.a, "/v1/days/2026-10-29/captures?limit=1&cursor="+next(page), 200)
			if string(items(page)[0]["speaker_ref"]) != `"person-1"` {
				t.Fatal("capture structure")
			}
			page = h.get(h.a, "/v1/threads?limit=1", 200)
			if len(items(page)) != 1 || string(items(page)[0]["session_id"]) != `"s-1"` || next(page) == "" {
				t.Fatal("thread filtering/order")
			}
			cursor := next(page)
			h.mu.Lock()
			slices.Reverse(h.threads)
			h.threads = append(h.threads, thread{"s-2", h.a.Owner.EndUser, "xinli-assistant"})
			h.mu.Unlock()
			page = h.get(h.a, "/v1/threads?limit=1&cursor="+cursor, 200)
			if string(items(page)[0]["session_id"]) != `"s-2"` {
				t.Fatal("thread cursor moved with kernel ordering")
			}
			page = h.get(h.a, "/v1/threads/s-1/events?limit=2", 200)
			if len(items(page)) != 2 || string(items(page)[1]["seq"]) != `"2"` {
				t.Fatal("event pagination")
			}
			h.get(h.a, "/v1/threads/s-3/events?limit=2&cursor="+next(page), 400)
			page = h.get(h.a, "/v1/threads/s-1/events?limit=2&cursor="+next(page), 200)
			if len(items(page)) != 1 || string(items(page)[0]["seq"]) != `"3"` {
				t.Fatal("event continuation")
			}
			quota := h.get(h.a, "/v1/entitlements", 200)
			if strings.Contains(string(quota["periods"]), "business_line\",\"limit") || strings.Contains(string(quota["periods"]), "999999") || !strings.Contains(string(quota["periods"]), "plus") {
				t.Fatal("quota scope/tier")
			}
			h.mu.Lock()
			h.kernelStatus = 500
			h.mu.Unlock()
			h.get(h.a, "/v1/threads", 503)
			h.mu.Lock()
			h.kernelStatus = 0
			h.deleteDuringEvents = true
			h.mu.Unlock()
			h.get(h.a, "/v1/threads/s-1/events", 404)
		})
	}
}

func TestValidationAndLifecycle(t *testing.T) {
	for _, kind := range []string{"memory", "pg"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, kind)
			for _, path := range []string{"/v1/projects?limit=0", "/v1/projects?limit=101", "/v1/projects?limit=-1", "/v1/projects?limit=1&limit=2", "/v1/projects?cursor=invalid", "/v1/projects?end_user=guess", "/v1/threads?end_user=guess", "/v1/tasks?limit=no", "/v1/days/2026-02-30", "/v1/commitments?status=unknown", "/v1/commitments?due_from=2026-10-31&due_to=2026-10-30", "/v1/today?date=2026-10-28", "/v1/entitlements?end_user=guess"} {
				h.get(h.a, path, 400)
			}
			h.get(h.a, "/v1/days/2026-10-28", 404)
			h.get(h.a, "/v1/days/2026-10-28/captures", 404)
			h.now = h.now.AddDate(0, 0, 9)
			h.clock.Store(h.now.UnixNano())
			h.get(h.a, "/v1/days/2026-10-29/captures", 410)
			day := h.get(h.a, "/v1/days/2026-10-29", 200)
			if string(day["expired"]) != "true" || string(day["capture_revision"]) != "1" {
				t.Fatal("expiration must be visible before cleanup")
			}
			h.get(h.a, "/v1/commitments/c-1/history", 200)
			if err := h.books.Update(h.ctx, h.a.Owner.BusinessLine, h.a.Owner.EndUser, func(book *domain.Book) error { _, err := book.EraseDay("2026-10-29", h.now); return err }); err != nil {
				t.Fatal(err)
			}
			h.get(h.a, "/v1/days/2026-10-29", 410)
			deletions := h.get(h.a, "/v1/deletions", 200)
			if len(items(deletions)) != 1 || string(items(deletions)[0]["status"]) != `"deleting"` {
				t.Fatal("deletion progress")
			}
			if _, err := h.books.BeginOwnerDeletion(h.ctx, h.a.Owner.BusinessLine, h.a.Owner.EndUser, h.now); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/v1/projects", "/v1/threads", "/v1/tasks", "/v1/entitlements", "/v1/days/2026-10-29/captures"} {
				h.get(h.a, path, 403)
			}
			if _, err := h.accounts.BeginDeletion(h.ctx, h.a.Owner); err != nil {
				t.Fatal(err)
			}
			h.get(h.a, "/v1/projects", 401)
			h.get(h.b, "/v1/projects", 200)
		})
	}
}

func TestTaskQueries(t *testing.T) {
	for _, kind := range []string{"memory", "pg"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, kind)
			var ids []string
			for i, owner := range []task.Owner{h.a.Owner, h.b.Owner, h.a.Owner, h.other.Owner} {
				source := task.Source{Owner: owner, Kind: "schedule", ID: fmt.Sprintf("day/%d", i), Version: 1}
				spec := task.Spec{Operation: "day", Target: fmt.Sprintf("day/%d", i), Date: "2026-10-29"}
				if err := h.tasks.Receive(h.ctx, source, []task.Spec{spec}, h.now); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, task.Identity(source, spec))
			}
			page := h.get(h.a, "/v1/tasks?limit=1", 200)
			if len(items(page)) != 1 || string(items(page)[0]["id"]) != strconv.Quote(ids[0]) {
				t.Fatal("task page")
			}
			page = h.get(h.a, "/v1/tasks?limit=1&cursor="+next(page), 200)
			if len(items(page)) != 1 || string(items(page)[0]["id"]) != strconv.Quote(ids[2]) {
				t.Fatal("task owner/order")
			}
			h.get(h.a, "/v1/tasks/"+ids[1], 404)
			h.get(h.a, "/v1/tasks/"+ids[3], 404)
			result := h.get(h.a, "/v1/tasks/"+ids[0], 200)
			if string(result["status"]) != `"processing"` || result["lease_generation"] != nil || result["input_id"] != nil {
				t.Fatal("task product status")
			}
			claimed, ok, err := h.tasks.Claim(h.ctx, ids[0], h.now, time.Minute)
			if err != nil || !ok {
				t.Fatalf("claim: %v", err)
			}
			claimed.State = task.Succeeded
			if err := h.tasks.Save(h.ctx, claimed, h.now, true); err != nil {
				t.Fatal(err)
			}
			if got := h.get(h.a, "/v1/tasks/"+ids[0], 200); string(got["status"]) != `"completed"` {
				t.Fatal("completed task")
			}
		})
	}
}

func TestCaptureCursorSurvivesErasure(t *testing.T) {
	for _, kind := range []string{"memory", "pg"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, kind)
			page := h.get(h.a, "/v1/days/2026-10-29/captures?limit=1", 200)
			cursor := next(page)
			if err := h.books.Update(h.ctx, h.a.Owner.BusinessLine, h.a.Owner.EndUser, func(book *domain.Book) error {
				_, err := book.Erase("2026-10-29", "1.0", "1.0", h.now)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			page = h.get(h.a, "/v1/days/2026-10-29/captures?limit=1&cursor="+cursor, 200)
			if len(items(page)) != 1 || string(items(page)[0]["text"]) != `"a second"` || string(page["capture_revision"]) != "2" {
				t.Fatal("erasure shifted capture cursor")
			}
		})
	}
}
