package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"persona/domain"
	"persona/mcpserver"
	"persona/store/memstore"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type created struct{ bl, eu, agent, sid string }
type submission struct{ sid, id, text string }
type fakeKernel struct {
	mu       sync.Mutex
	sessions []created
	attempts []submission
	inputs   map[string]submission
	block    <-chan struct{}
	started  chan struct{}
	failText string
}

func (k *fakeKernel) CreateSession(ctx context.Context, bl, eu, agent string) (string, error) {
	if k.started != nil {
		select {
		case k.started <- struct{}{}:
		default:
		}
	}
	if k.block != nil {
		select {
		case <-k.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	sid := fmt.Sprintf("ses_%d", len(k.sessions)+1)
	k.sessions = append(k.sessions, created{bl, eu, agent, sid})
	return sid, nil
}

func (k *fakeKernel) Submit(ctx context.Context, sid, id, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.attempts = append(k.attempts, submission{sid, id, text})
	if text == k.failText {
		return errors.New("submit failed")
	}
	if k.inputs == nil {
		k.inputs = map[string]submission{}
	}
	k.inputs[sid+"/"+id] = submission{sid, id, text}
	return nil
}

func finish(t *testing.T, o *Orchestrator) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := o.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestChangesetsAndCollect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	books := memstore.New()
	unblock := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(unblock) })
	k := &fakeKernel{block: unblock, started: make(chan struct{}, 1)}
	o := New(ctx, books, k, Options{Now: func() time.Time { return now }})
	server := httptest.NewServer(mcpserver.Handler(books, mcpserver.Options{Now: func() time.Time { return now }, Notifier: o}))
	defer server.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	conn, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	meta := mcp.Meta{"yanshi/business_line": "bl", "yanshi/end_user": "u", "yanshi/session": "day", "yanshi/captures": []any{
		map[string]any{"ref": "#2.0", "speaker": "wearer", "spoken_at": now},
		map[string]any{"ref": "#2.1", "speaker": "other", "spoken_at": now},
	}}
	call := func(name string, args any) {
		t.Helper()
		result, err := conn.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args, Meta: meta})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("MCP error: %+v", result)
		}
	}
	call("file_changes", map[string]any{
		"source":    map[string]string{"from": "#2.0", "to": "#2.1"},
		"exchanges": map[string]any{"create": []any{map[string]any{"from": "#2.0", "to": "#2.1", "title": "报价", "summary": "约好报价", "projects": []any{map[string]string{"match": "new", "name": "装修"}}}}},
	})
	call("file_day", map[string]any{"day": map[string]string{"headline": "装修定稿"}})
	// CreateSession 仍被阻塞时 MCP 已返回，证明通知不等待归集网络请求。
	select {
	case <-k.started:
	case <-ctx.Done():
		t.Fatal("collection did not start")
	}
	for range 8 {
		call("file_day", map[string]any{"day": map[string]string{"headline": "装修定稿"}})
	}
	once.Do(func() { close(unblock) })
	finish(t, o)
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.sessions) != 1 || k.sessions[0].agent != ProjectAgent || k.sessions[0].bl != "bl" || k.sessions[0].eu != "u" {
		t.Fatalf("sessions: %+v", k.sessions)
	}
	if len(k.attempts) != 9 || len(k.inputs) != 1 {
		t.Fatalf("attempts=%d unique=%d", len(k.attempts), len(k.inputs))
	}
	want := submission{k.sessions[0].sid, "xinli_day_" + domain.NewProject, "整理 2026-10-23"}
	for _, got := range k.attempts {
		if got != want {
			t.Fatalf("submit: %+v", got)
		}
	}
	if err := books.View(ctx, "bl", "u", func(b *domain.Book) error {
		ps := b.State().Projects
		if len(ps) != 1 || ps[0].Session != want.sid {
			t.Errorf("binding: %+v", ps)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCollectionIgnoresRequestCancellation(t *testing.T) {
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	books := memstore.New()
	if err := books.Update(life, "bl", "u", func(b *domain.Book) error {
		b.Seed(domain.SeedData{Projects: []domain.Project{{ID: "p"}}}, time.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	k := &fakeKernel{}
	o := New(life, books, k, Options{})
	request, stop := context.WithCancel(life)
	stop()
	o.DayFiled(request, "bl", "u", "day", "2026-10-23", []string{"missing", "p"})
	finish(t, o)
	if len(k.inputs) != 1 {
		t.Fatalf("inputs=%d", len(k.inputs))
	}
	o.DayFiled(life, "bl", "u", "late", "2026-10-23", []string{"p"})
	finish(t, o)
	if len(k.inputs) != 1 {
		t.Fatal("accepted notification after shutdown")
	}
}

func TestWeeklyReview(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	books := memstore.New()
	if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
		b.Seed(domain.SeedData{Projects: []domain.Project{{ID: "P-0001"}, {ID: "old"}}, Exchanges: []domain.SeedExchange{{Date: "2026-10-24", Project: "P-0001"}, {Date: "2026-08-01", Project: "old"}}}, now)
		b.Bind("P-0001", "ses_p1")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	k := &fakeKernel{}
	o := New(ctx, books, k, Options{Now: func() time.Time { return now }})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sid, err := o.Week(ctx, "bl", "u")
			if err != nil || sid != "ses_1" {
				t.Errorf("Week=%s %v", sid, err)
			}
		}()
	}
	wg.Wait()
	finish(t, o)
	if len(k.sessions) != 1 || k.sessions[0].agent != WeekAgent {
		t.Fatalf("sessions: %+v", k.sessions)
	}
	for _, want := range []submission{{"ses_1", "xinli_week_2026-10-19", "回顾本周"}, {"ses_p1", "xinli_rewrite_2026-10-19_P-0001", "重写现状"}} {
		if got := k.inputs[want.sid+"/"+want.id]; got != want {
			t.Fatalf("got=%+v want=%+v", got, want)
		}
	}
	if len(k.inputs) != 2 {
		t.Fatalf("unique inputs: %d", len(k.inputs))
	}
	if err := books.View(ctx, "bl", "u", func(b *domain.Book) error {
		if !b.State().Projects[1].Archived {
			t.Error("old project not archived")
		}
		if b.ReviewWeek(now).Session != "ses_1" {
			t.Error("week not bound")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWeekSubmitFailureKeepsBinding(t *testing.T) {
	ctx := context.Background()
	k := &fakeKernel{failText: "回顾本周"}
	o := New(ctx, memstore.New(), k, Options{})
	if _, err := o.Week(ctx, "bl", "u"); err == nil {
		t.Fatal("missing error")
	}
	k.failText = ""
	if _, err := o.Week(ctx, "bl", "u"); err != nil {
		t.Fatal(err)
	}
	finish(t, o)
	if len(k.sessions) != 1 {
		t.Fatal("retry created a second session")
	}
}

func TestRewriteFailureDoesNotFailReview(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	books := memstore.New()
	if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
		b.Seed(domain.SeedData{Projects: []domain.Project{{ID: "p"}}, Exchanges: []domain.SeedExchange{{Date: "2026-10-24", Project: "p"}}}, now)
		b.Bind("p", "project-session")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	k := &fakeKernel{failText: "重写现状"}
	o := New(ctx, books, k, Options{Now: func() time.Time { return now }})
	sid, err := o.Week(ctx, "bl", "u")
	if err != nil || sid != "ses_1" {
		t.Fatalf("review: %q %v", sid, err)
	}
	finish(t, o)
	if len(k.attempts) != 2 || len(k.inputs) != 1 {
		t.Fatalf("attempts=%d inputs=%d", len(k.attempts), len(k.inputs))
	}
}

func TestShutdownWaitsForCollection(t *testing.T) {
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	books := memstore.New()
	if err := books.Update(life, "bl", "u", func(b *domain.Book) error {
		b.Seed(domain.SeedData{Projects: []domain.Project{{ID: "p"}}}, time.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	unblock := make(chan struct{})
	defer close(unblock)
	k := &fakeKernel{block: unblock, started: make(chan struct{}, 1)}
	o := New(life, books, k, Options{})
	o.DayFiled(life, "bl", "u", "day", "2026-10-23", []string{"p"})
	select {
	case <-k.started:
	case <-time.After(5 * time.Second):
		t.Fatal("collection did not start")
	}
	expired, stop := context.WithCancel(context.Background())
	stop()
	if err := o.Shutdown(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown: %v", err)
	}
	// 超时后取消进程生命周期，阻塞中的内核调用退出，后续等待仍能正常结束。
	cancel()
	finish(t, o)
}
