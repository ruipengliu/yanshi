package account_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"persona/domain"
	"persona/store"
	"xinli/capture"
	"xinli/capture/sender"
)

type captureClockStore struct {
	store.Store
	now time.Time
}

func (s captureClockStore) Update(ctx context.Context, bl, eu string, fn func(*domain.Book) error) error {
	return s.Store.Update(ctx, bl, eu, func(b *domain.Book) error { b.SetCaptureClock(func() time.Time { return s.now }); return fn(b) })
}
func (s captureClockStore) View(ctx context.Context, bl, eu string, fn func(*domain.Book) error) error {
	return s.Store.View(ctx, bl, eu, func(b *domain.Book) error { b.SetCaptureClock(func() time.Time { return s.now }); return fn(b) })
}
func (s captureClockStore) ViewPlanned(ctx context.Context, bl, eu string, plan domain.ReadPlan, fn func(*domain.Book) error) error {
	return s.Store.ViewPlanned(ctx, bl, eu, plan, func(b *domain.Book) error { b.SetCaptureClock(func() time.Time { return s.now }); return fn(b) })
}

type captureSend func(context.Context, capture.Batch) (capture.Receipt, error)

func (f captureSend) Send(ctx context.Context, b capture.Batch) (capture.Receipt, error) {
	return f(ctx, b)
}

func TestPersistentCaptureDelivery(t *testing.T) {
	t.Run("legacy", func(t *testing.T) { persistentCaptureDelivery(t, false) })
	t.Run("metadata", func(t *testing.T) { persistentCaptureDelivery(t, true) })
}

func persistentCaptureDelivery(t *testing.T, metadata bool) {
	t.Helper()
	each(t, func(t *testing.T, b *backend) {
		now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		ctx := context.Background()
		accounts := service(b, &now)
		credentials := login(t, accounts, "phone")
		books := func() store.Store { return captureClockStore{b.persona, now} }
		server := httptest.NewServer(capture.Handler(books(), accounts))
		defer func() { server.Close() }()
		client := capture.Client{BaseURL: server.URL, Token: func(context.Context) (string, error) { return credentials.AccessToken, nil }}
		path := filepath.Join(t.TempDir(), "sender.json")
		id := sender.Identity{BusinessLine: credentials.Owner.BusinessLine, EndUser: credentials.Owner.EndUser, DeviceID: "phone", SessionID: "recording"}
		host, err := sender.Open(path, id)
		require(t, err)
		defer func() { host.Close() }()
		segments := []capture.Segment{{Speaker: "wearer", Text: "午夜前积压", At: "2026-10-27T23:59:00Z"}}
		if metadata {
			segments[0].SpeakerGrade = "timeline"
			segments[0].Disposition = "accept"
			segments[0].Text = "甲🙂明天交稿"
			segments[0].LowSupportSpans = []domain.LowSupportSpan{{Start: 1, End: 2, CriticalKinds: []string{"none"}}, {Start: 2, End: 4}}
		}
		first, err := host.Enqueue("2026-10-27", "UTC", 0, segments)
		require(t, err)
		segments = []capture.Segment{{Speaker: "other-1", SpeakerRef: "P-1", Text: "午夜后积压", At: "2026-10-28T00:01:00Z"}}
		if metadata {
			segments[0].Speaker, segments[0].SpeakerRef = "unknown", ""
			segments[0].Disposition, segments[0].DispositionReason = "quarantine", "suspected_replay"
			segments[0].Background = true
		}
		second, err := host.Enqueue("2026-10-28", "UTC", 0, segments)
		require(t, err)
		offline := errors.New("offline")
		err = host.Flush(ctx, captureSend(func(context.Context, capture.Batch) (capture.Receipt, error) { return capture.Receipt{}, offline }))
		if !errors.Is(err, offline) {
			t.Fatal(err)
		}
		var attempted capture.Batch
		err = host.Flush(ctx, captureSend(func(ctx context.Context, in capture.Batch) (capture.Receipt, error) {
			attempted = in
			_, err := client.Send(ctx, in)
			if err != nil {
				return capture.Receipt{}, err
			}
			return capture.Receipt{}, offline
		}))
		if !errors.Is(err, offline) || attempted.CaptureID != first.CaptureID || attempted.Seq != first.Seq {
			t.Fatal(err)
		}
		require(t, host.Close())
		server.Close()
		// 应用与 Persona 连接全部重开，不能借进程内缓存恢复去重。
		b.reopen()
		accounts = service(b, &now)
		server = httptest.NewServer(capture.Handler(books(), accounts))
		client.BaseURL = server.URL
		host, err = sender.Open(path, id)
		require(t, err)
		calls := 0
		require(t, host.Flush(ctx, captureSend(func(ctx context.Context, in capture.Batch) (capture.Receipt, error) {
			calls++
			expected := first
			if calls == 2 {
				expected = second
			}
			if !reflect.DeepEqual(in, expected) {
				t.Fatal("request identity or metadata changed")
			}
			return client.Send(ctx, in)
		})))
		if calls != 2 {
			t.Fatal(calls)
		}
		for _, entry := range host.State().Entries {
			if entry.Status != "accepted" || len(entry.Batch.Segments) != 0 || len(entry.Receipt.Refs) != 1 || entry.Receipt.Refs[0] != "1.0" {
				t.Fatal(entry.Status)
			}
		}
		require(t, host.Flush(ctx, captureSend(func(context.Context, capture.Batch) (capture.Receipt, error) {
			t.Fatal("confirmed batch resent")
			return capture.Receipt{}, nil
		})))
		require(t, host.Close())
		host, err = sender.Open(path, id)
		require(t, err)
		for _, batch := range []capture.Batch{first, second} {
			require(t, books().View(ctx, id.BusinessLine, id.EndUser, func(book *domain.Book) error {
				segments, err := book.CaptureAccess().Batch(batch.BusinessDay, 1)
				if err == nil && (len(segments) != 1 || !segments[0].Equal(batch.Ordered().Segments[0])) {
					t.Fatal("duplicate, lost batch or metadata")
				}
				return err
			}))
		}
		if metadata {
			changed := first
			changed.Segments = capture.CloneSegments(first.Segments)
			changed.Segments[0].LowSupportSpans[0].CriticalKinds[0] = "number"
			if _, err := client.Send(ctx, changed); !errors.Is(err, capture.ErrRejected) {
				t.Fatal("metadata missing from duplicate digest", err)
			}
		}
	})
}

func TestCaptureHTTPGenerationAndAuthentication(t *testing.T) {
	each(t, func(t *testing.T, b *backend) {
		now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		ctx := context.Background()
		accounts := service(b, &now)
		credential := login(t, accounts, "phone")
		secondDevice := login(t, accounts, "tablet")
		books := captureClockStore{b.persona, now}
		server := httptest.NewServer(capture.Handler(books, accounts))
		defer server.Close()
		token := credential.AccessToken
		client := capture.Client{BaseURL: server.URL, Token: func(context.Context) (string, error) { return token, nil }}
		batch := func(seq uint64, id string) capture.Batch {
			return capture.Batch{SessionID: "recording", Seq: seq, CaptureID: id, TimeZone: "UTC", Generation: 0, Segments: []capture.Segment{{Speaker: "wearer", Text: "正文", At: "2026-10-28T10:00:00Z"}}}
		}
		third := batch(3, "third")
		out, err := client.Send(ctx, third)
		if err != nil || out.Status != "staged" || out.NextSeq != 1 || len(out.Refs) != 0 {
			t.Fatal(out, err)
		}
		first := batch(1, "first")
		out, err = client.Send(ctx, first)
		if err != nil || out.Status != "accepted" || out.Refs[0] != "1.0" {
			t.Fatal(out, err)
		}
		token = secondDevice.AccessToken
		if _, err := client.Send(ctx, batch(2, "second")); !errors.Is(err, capture.ErrRejected) {
			t.Fatal("other device accepted", err)
		}
		token = credential.AccessToken
		bad := first
		bad.Segments = []capture.Segment{{Speaker: "wearer", Text: "异文", At: "2026-10-28T10:00:00Z"}}
		if _, err := client.Send(ctx, bad); !errors.Is(err, capture.ErrRejected) {
			t.Fatal(err)
		}
		require(t, books.Update(ctx, credential.Owner.BusinessLine, credential.Owner.EndUser, func(book *domain.Book) error { _, err := book.Erase("2026-10-28", "1.0", "1.0", now); return err }))
		out, err = client.Send(ctx, batch(2, "second"))
		if !errors.Is(err, capture.ErrResync) || out.Generation != 1 || out.Status != "resync_required" {
			t.Fatal(out, err)
		}
		for _, raw := range []string{`{}`, `{"generation":0,"owner":"forged"}`, `{"generation":0} {}`} {
			req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/captures/batches", strings.NewReader(raw))
			req.Header.Set("Authorization", "Bearer "+token)
			response, err := http.DefaultClient.Do(req)
			require(t, err)
			response.Body.Close()
			if response.StatusCode != 400 {
				t.Fatal(response.StatusCode)
			}
		}
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/captures/sync?business_day=2026-10-28&session_id=recording", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(req)
		require(t, err)
		var sync capture.Receipt
		require(t, json.NewDecoder(response.Body).Decode(&sync))
		response.Body.Close()
		if sync.Generation != 1 || sync.Status != "resync_required" {
			t.Fatal(sync)
		}
		require(t, accounts.Logout(ctx, credential.Owner, "phone"))
		req, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/captures/batches", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+token)
		response, err = http.DefaultClient.Do(req)
		require(t, err)
		response.Body.Close()
		if response.StatusCode != 401 {
			t.Fatal("revoked capture access", response.StatusCode)
		}
	})
}

type captureRejectedCommit struct{ store.Store }

func (s captureRejectedCommit) Update(ctx context.Context, bl, eu string, fn func(*domain.Book) error) error {
	if err := s.Store.View(ctx, bl, eu, fn); err != nil {
		return err
	}
	return errors.New("commit failed")
}

func TestCaptureDoesNotReturnUncommittedReferences(t *testing.T) {
	each(t, func(t *testing.T, b *backend) {
		now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		accounts := service(b, &now)
		credentials := login(t, accounts, "phone")
		books := captureRejectedCommit{captureClockStore{b.persona, now}}
		handler := capture.Handler(books, accounts)
		raw := `{"session_id":"s","seq":1,"capture_id":"c","time_zone":"UTC","generation":0,"segments":[{"speaker":"wearer","text":"文字","at":"2026-10-28T10:00:00Z"}]}`
		request := httptest.NewRequest(http.MethodPost, "/v1/captures/batches", strings.NewReader(raw))
		request.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var out capture.Error
		require(t, json.Unmarshal(response.Body.Bytes(), &out))
		if response.Code != 503 || out.Code != "unavailable" || out.Status == "accepted" || len(out.Refs) != 0 {
			t.Fatal("uncommitted references escaped")
		}
	})
}

func TestCaptureHTTPMetadataRoundtrip(t *testing.T) {
	each(t, func(t *testing.T, b *backend) {
		now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		accounts := service(b, &now)
		credentials := login(t, accounts, "phone")
		books := captureClockStore{b.persona, now}
		h := capture.Handler(books, accounts)
		batch := capture.Batch{SessionID: "metadata", CaptureID: "metadata", Seq: 1, BusinessDay: "2026-10-28", TimeZone: "UTC", Segments: []capture.Segment{
			{Speaker: "wearer", SpeakerGrade: "evidence", Disposition: "accept", Text: "甲🙂明天交稿", At: "2026-10-28T10:00:00Z", LowSupportSpans: []domain.LowSupportSpan{{Start: 1, End: 2, CriticalKinds: []string{"none"}}, {Start: 2, End: 4, CriticalKinds: []string{"date"}}}},
			{Speaker: "other", Text: "他人原话", At: "2026-10-28T10:01:00Z"},
			{Speaker: "other-1", SpeakerRef: "P-1", Background: true, Text: "背景原话", At: "2026-10-28T10:02:00Z"},
			{Speaker: "unknown", Disposition: "quarantine", DispositionReason: "hallucination_disagreement", Text: "隔离原话", At: "2026-10-28T10:03:00Z"},
		}}
		raw, err := json.Marshal(batch)
		require(t, err)
		var first capture.Receipt
		for range 2 {
			r := httptest.NewRequest("POST", "/v1/captures/batches", strings.NewReader(string(raw)))
			r.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var out capture.Receipt
			require(t, json.Unmarshal(w.Body.Bytes(), &out))
			if w.Code != 200 || len(out.Refs) != len(batch.Segments) || first.Status != "" && !reflect.DeepEqual(first, out) {
				t.Fatalf("metadata write/retry: %d %s", w.Code, w.Body.String())
			}
			first = out
		}
		require(t, books.View(context.Background(), credentials.Owner.BusinessLine, credentials.Owner.EndUser, func(book *domain.Book) error {
			got, err := book.CaptureAccess().Batch(batch.BusinessDay, 1)
			if err != nil {
				return err
			}
			want := batch.Ordered().Segments
			if len(got) != len(want) || book.BusinessDay(batch.BusinessDay).CaptureRevision != 1 {
				t.Fatal("metadata lost or duplicate appended")
			}
			for i := range want {
				if !got[i].Equal(want[i]) {
					t.Fatal("metadata changed")
				}
			}
			return nil
		}))
	})
}

func TestInvalidCaptureMetadataRejected(t *testing.T) {
	each(t, func(t *testing.T, b *backend) {
		now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		accounts := service(b, &now)
		credentials := login(t, accounts, "phone")
		books := captureClockStore{b.persona, now}
		h := capture.Handler(books, accounts)
		for _, fields := range []string{
			`"speaker":"invalid"`,
			`"speaker":"wearer","speaker_grade":"invalid"`,
			`"speaker":"wearer","disposition":"invalid"`,
			`"speaker":"wearer","disposition_reason":"playback"`,
			`"speaker":"wearer","background":true`,
			`"speaker":"wearer","low_support_spans":[{"start":0,"end":3}]`,
			`"speaker":"wearer","unexpected":false`,
			`"speaker":"wearer","disposition":"quarantine"`,
			`"speaker":"wearer","low_support_spans":[{"start":0,"end":1,"critical_kinds":["bad"]}]`,
			`"speaker":"wearer","low_support_spans":[{"start":0,"end":1,"unexpected":0}]`,
			`"speaker":"unknown","speaker_ref":"P-1"`,
			`"speaker":"other","speaker_grade":"evidence"`,
		} {
			body := `{"session_id":"blocked","seq":2,"capture_id":"blocked","business_day":"2026-10-27","time_zone":"UTC","generation":0,"segments":[{"speaker":"wearer","text":"合法前段","at":"2026-10-27T12:00:00Z"},{"text":"原文","at":"2026-10-27T12:01:00Z",` + fields + `}]}`
			r := httptest.NewRequest("POST", "/v1/captures/batches", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_batch") {
				t.Fatalf("status=%d: %s", w.Code, w.Body.String())
			}
			require(t, books.View(context.Background(), credentials.Owner.BusinessLine, credentials.Owner.EndUser, func(book *domain.Book) error {
				segments, err := book.DayCaptures("2026-10-27")
				if err != nil {
					return err
				}
				if len(segments) != 0 || !book.BusinessDay("2026-10-27").ExpiresAt.IsZero() {
					t.Fatal("invalid batch stored")
				}
				pending, err := book.CaptureAccess().Pending("2026-10-27", "blocked", 2, 0)
				if len(pending) != 0 {
					t.Fatal("invalid batch staged")
				}
				return err
			}))
		}
	})
}
