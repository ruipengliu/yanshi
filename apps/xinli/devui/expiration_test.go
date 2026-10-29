package devui

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"persona/domain"
	"persona/store/memstore"
	"xinli/kernel"
)

func TestExpiredCaptureRoutesAndInflightBrief(t *testing.T) {
	const date = "2026-10-28"
	expires, _ := domain.CaptureExpiresAt(date, "Asia/Shanghai")
	now := expires.Add(-time.Second)
	books := memstore.New(func() time.Time { return now })
	if err := books.Update(context.Background(), "bl", "u", func(b *domain.Book) error {
		_, err := b.AppendCaptures(date, "a", []domain.CaptureSegment{{Speaker: "wearer", Text: "私密原文"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	a := &briefActions{call: func(context.Context, string, string, string, string) (kernel.OneshotResult, error) {
		calls++
		now = expires
		return kernel.OneshotResult{Output: json.RawMessage(`{"brief":"私密简述"}`)}, nil
	}}
	h := Handler(Config{}, Options{Store: books, Kernel: a, Now: func() time.Time { return now }})
	metadata := httptest.NewRecorder()
	h.ServeHTTP(metadata, httptest.NewRequest("GET", "/v1/dev/bl/u/captures?date="+date, nil))
	var state struct {
		Day domain.BusinessDay `json:"day"`
	}
	if err := json.Unmarshal(metadata.Body.Bytes(), &state); err != nil || metadata.Code != 200 || state.Day.CaptureRevision != 1 || !state.Day.ExpiresAt.Equal(expires) {
		t.Fatal("capture lifetime metadata missing", metadata.Code, err)
	}
	for _, request := range []struct{ method, path, body string }{
		{"POST", "/brief", `{"date":"2026-10-28"}`},
		{"GET", "/captures?date=2026-10-28", ""},
		{"POST", "/captures", `{"date":"2026-10-28","segments":[{"speaker":"wearer","text":"补传"}]}`},
		{"POST", "/brief", `{"date":"2026-10-28"}`},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(request.method, "/v1/dev/bl/u"+request.path, strings.NewReader(request.body)))
		if w.Code != 409 || !strings.Contains(w.Body.String(), "已到期") || strings.Contains(w.Body.String(), "私密") {
			t.Fatal(request.path, w.Code, w.Body.String())
		}
	}
	if calls != 1 {
		t.Fatal("expired input reached model", calls)
	}
}
