package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"yanshi/internal/auth"
	"yanshi/internal/clock"
	"yanshi/internal/httpapi"
	"yanshi/internal/outbox"
	"yanshi/internal/outbox/memoutbox"
	"yanshi/internal/outbox/pgoutbox"
	"yanshi/internal/pg/pgtest"
)

func TestOutboxHTTPAuthorizationAndErrors(t *testing.T) {
	e := newAuthzEnv(t)
	store := e.api.Outbox.(*memoutbox.Store)
	for _, bl := range []string{"demo", "other"} {
		env := outbox.Envelope{EnvelopeID: outbox.ID(bl, 1, outbox.Question), BusinessLine: bl, SessionID: bl, Seq: 1, Kind: outbox.Question}
		if err := store.Commit(context.Background(), []outbox.Envelope{env}, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	var cursor string
	for _, bl := range []string{"demo", "other"} {
		code, body := e.do(t, "GET", "/v1/outbox?limit=1", e.token(t, bl+"/", time.Hour), nil)
		var page outbox.Page
		if code != 200 || json.Unmarshal([]byte(body), &page) != nil || len(page.Events) != 1 || page.Events[0].BusinessLine != bl {
			t.Fatalf("tenant %s: %d %s", bl, code, body)
		}
		if !strings.Contains(body, `"envelope_id"`) || strings.Contains(body, `"EnvelopeID"`) {
			t.Fatal("incorrect wire fields")
		}
		if bl == "demo" {
			cursor = page.Next
		}
	}
	for _, test := range []struct {
		query, who, code string
		status           int
	}{
		{"?business_line=other", "demo/", "forbidden", 403},
		{"", "demo/u1", "forbidden", 403},
		{"?after=" + url.QueryEscape(cursor), "other/", "outbox_cursor_scope_mismatch", 403},
		{"?after=" + url.QueryEscape(cursor) + "&kinds=question", "demo/", "outbox_filter_changed", 409},
		{"?after=bad", "demo/", "outbox_invalid_cursor", 400},
		{"?limit=-1", "demo/", "outbox_invalid_request", 400},
		{"?limit=0", "demo/", "outbox_invalid_request", 400},
		{"?kinds=bad", "demo/", "outbox_invalid_request", 400},
		{"?shard=1", "demo/", "outbox_unknown_shard", 404},
	} {
		code, body := e.do(t, "GET", "/v1/outbox"+test.query, e.token(t, test.who, time.Hour), nil)
		if code != test.status || !strings.Contains(body, test.code) {
			t.Fatalf("%s: %d %s", test.query, code, body)
		}
	}
	if err := store.RotateEpoch(context.Background()); err != nil {
		t.Fatal(err)
	}
	code, body := e.do(t, "GET", "/v1/outbox?after="+url.QueryEscape(cursor), e.token(t, "demo/", time.Hour), nil)
	if code != 410 || !strings.Contains(body, "outbox_epoch_changed") {
		t.Fatalf("epoch=%d %s", code, body)
	}
	code, body = e.do(t, "GET", "/v1/outbox/shards?kinds=question", e.token(t, "demo/", time.Hour), nil)
	var shards struct {
		Shards []struct {
			Shard  int    `json:"shard"`
			Status string `json:"status"`
			Next   string `json:"next"`
		} `json:"shards"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &shards) != nil || len(shards.Shards) != 1 || shards.Shards[0].Next == "" {
		t.Fatalf("shards=%d %s", code, body)
	}
	// H 已绑定过滤版本，可直接用于随后追赶；空结果仍带游标。
	code, body = e.do(t, "GET", "/v1/outbox?kinds=question&after="+url.QueryEscape(shards.Shards[0].Next), e.token(t, "demo/", time.Hour), nil)
	var empty outbox.Page
	if code != 200 || json.Unmarshal([]byte(body), &empty) != nil || len(empty.Events) != 0 || empty.Next == "" {
		t.Fatalf("empty=%d %s", code, body)
	}
	// 先留下旧位置，再推进真正的保留地板，HTTP 必须明确返回过期。
	old, err := store.Head(context.Background(), outbox.Request{BusinessLine: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(context.Background(), nil, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	for _, at := range []time.Time{now, now.Add(outbox.Retention)} {
		if err := store.Maintain(context.Background(), clock.NewFake(at), 100); err != nil {
			t.Fatal(err)
		}
	}
	code, body = e.do(t, "GET", "/v1/outbox?after="+url.QueryEscape(old), e.token(t, "demo/", time.Hour), nil)
	if code != 410 || !strings.Contains(body, "outbox_cursor_expired") {
		t.Fatalf("expired=%d %s", code, body)
	}
}

type silentOutbox struct {
	outbox.Reader
	read chan struct{}
	once sync.Once
}

func (s *silentOutbox) Read(ctx context.Context, req outbox.Request) (outbox.Page, error) {
	page, err := s.Reader.Read(ctx, req)
	if err == nil && len(page.Events) == 0 {
		s.once.Do(func() { close(s.read) })
	}
	return page, err
}

func (s *silentOutbox) Subscribe(string) (<-chan struct{}, func()) { return nil, func() {} }

func TestOutboxLongPollRechecksWhenUnrelatedTransactionEnds(t *testing.T) {
	pool, _ := pgtest.Fresh(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT pg_current_xact_id()`); err != nil {
		t.Fatal(err)
	}
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(context.Background())
	env := outbox.Deleted("bl", "s", "service", "")
	if err := pgoutbox.Insert(ctx, writer, []outbox.Envelope{env}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	store := &silentOutbox{Reader: pgoutbox.Store{Pool: pool}, read: make(chan struct{})}
	srv := httptest.NewServer((&httpapi.Server{Outbox: store, OutboxWait: 8 * time.Second, OutboxRecheck: 10 * time.Millisecond}).Handler())
	defer srv.Close()
	type result struct {
		page outbox.Page
		code int
		err  error
	}
	completed := make(chan result, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/outbox?business_line=bl", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			completed <- result{err: err}
			return
		}
		defer resp.Body.Close()
		var page outbox.Page
		err = json.NewDecoder(resp.Body).Decode(&page)
		completed <- result{page: page, code: resp.StatusCode, err: err}
	}()
	select {
	case <-store.read:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// blocker 没写本业务线信封，结束时不会发出出站通知。
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-completed:
		if got.err != nil || got.code != 200 || len(got.page.Events) != 1 || got.page.Events[0] != env {
			t.Fatalf("response=%+v", got)
		}
	case <-ctx.Done():
		t.Fatal("periodic recheck did not release long poll")
	}
}

func TestOutboxLongPollTimeoutAndCancellation(t *testing.T) {
	store := memoutbox.New()
	api := &httpapi.Server{Outbox: store, OutboxWait: 30 * time.Millisecond, OutboxRecheck: 5 * time.Millisecond}
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/outbox?business_line=bl")
	if err != nil {
		t.Fatal(err)
	}
	var page outbox.Page
	err = json.NewDecoder(resp.Body).Decode(&page)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || page.Events == nil || len(page.Events) != 0 || page.Next == "" {
		t.Fatal("empty long poll lost progress")
	}
	blocked := &silentOutbox{Reader: store, read: make(chan struct{})}
	pending := httptest.NewServer((&httpapi.Server{Outbox: blocked, OutboxWait: time.Second}).Handler())
	defer pending.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, "GET", pending.URL+"/v1/outbox?business_line=bl", nil)
		response, err := http.DefaultClient.Do(req)
		if response != nil {
			response.Body.Close()
		}
		completed <- err
	}()
	select {
	case <-blocked.read:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	if err := <-completed; !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight cancellation=%v", err)
	}
}

type outboxExpiringToken struct{}

func (outboxExpiringToken) Verify(string) (auth.Principal, error) {
	return auth.Principal{BusinessLine: "bl", Expires: time.Now().Add(30 * time.Millisecond)}, nil
}

func TestOutboxLongPollStopsAtTokenExpiry(t *testing.T) {
	handler := (&httpapi.Server{Auth: outboxExpiringToken{}, Outbox: memoutbox.New(), OutboxWait: time.Second}).Handler()
	req := httptest.NewRequestWithContext(t.Context(), "GET", "/v1/outbox", nil)
	req.Header.Set("Authorization", "Bearer service")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	var body map[string]string
	if w.Code != http.StatusUnauthorized || json.Unmarshal(w.Body.Bytes(), &body) != nil ||
		body["code"] != "unauthenticated" || body["error"] != "unauthenticated" || w.Header().Get("WWW-Authenticate") != `Bearer error="invalid_token"` {
		t.Fatalf("expired token got %d %s %v", w.Code, w.Body, w.Header())
	}
}
