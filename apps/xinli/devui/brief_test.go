package devui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"xinli/kernel"

	"persona/domain"
	"persona/store/memstore"
)

type briefActions struct {
	devActions
	call func(context.Context, string, string, string, string) (kernel.OneshotResult, error)
}

func (a *briefActions) OneshotForBusinessLine(ctx context.Context, bl, eu, name, input string) (kernel.OneshotResult, error) {
	return a.call(ctx, bl, eu, name, input)
}

func TestBriefRoutes(t *testing.T) {
	for _, change := range []string{"none", "erase", "erase_day", "append"} {
		t.Run(change, func(t *testing.T) {
			books := memstore.New()
			update := func(fn func(*domain.Book) error) {
				t.Helper()
				if err := books.Update(context.Background(), "bl", "u", fn); err != nil {
					t.Fatal(err)
				}
			}
			update(func(b *domain.Book) error {
				_, err := b.AppendCaptures("2026-10-28", "capture", []domain.CaptureSegment{
					{Speaker: "wearer", Text: "我来做\n报价"},
					{Speaker: "other", SpeakerKey: "other-1", SpeakerRef: "P-0012", Text: "明天给我"},
				})
				return err
			})
			a := &briefActions{call: func(_ context.Context, bl, eu, name, input string) (kernel.OneshotResult, error) {
				if bl != "bl" || eu != "u" || name != "xinli-live-brief" || input != "[佩戴者] 我来做 报价\n[他人 other-1 @P-0012] 明天给我" {
					t.Fatalf("incorrect request: %q", input)
				}
				// 在网络返回点插入原文变更；若持有 Persona 事务锁，此处也会暴露死锁。
				update(func(b *domain.Book) error {
					switch change {
					case "erase":
						_, err := b.Erase("2026-10-28", "1.0", "1.0", time.Now())
						return err
					case "erase_day":
						_, err := b.EraseDay("2026-10-28", time.Now())
						return err
					case "append":
						_, err := b.AppendCaptures("2026-10-28", "next", []domain.CaptureSegment{{Speaker: "wearer", Text: "新增"}})
						return err
					}
					return nil
				})
				return kernel.OneshotResult{Output: json.RawMessage(`{"brief":"私密简述"}`)}, nil
			}}
			h := Handler(Config{}, Options{Store: books, Kernel: a})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/dev/bl/u/brief", strings.NewReader(`{"date":"2026-10-28"}`)))
			if change == "none" {
				if w.Code != 200 || !strings.Contains(w.Body.String(), "私密简述") {
					t.Fatal(w.Code, w.Body.String())
				}
			} else if w.Code != 409 || strings.Contains(w.Body.String(), "私密简述") || !strings.Contains(w.Body.String(), "原文已变化") {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}

func TestBriefErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		output string
		status int
	}{
		{&kernel.HTTPError{StatusCode: 429}, "", 429},
		{&kernel.HTTPError{StatusCode: 503}, "", 503},
		{&kernel.HTTPError{StatusCode: 504}, "", 504},
		{&kernel.HTTPError{StatusCode: 422}, "", 422},
		{errors.New("private-content"), "", 502},
		{context.DeadlineExceeded, "", 504},
		{nil, `{"unexpected":"private-content"}`, 502},
	} {
		books := memstore.New()
		if err := books.Update(context.Background(), "bl", "u", func(b *domain.Book) error {
			_, err := b.AppendCaptures("2026-10-28", "c", []domain.CaptureSegment{{Speaker: "wearer", Text: "原文"}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		a := &briefActions{call: func(context.Context, string, string, string, string) (kernel.OneshotResult, error) {
			return kernel.OneshotResult{Output: json.RawMessage(tc.output)}, tc.err
		}}
		w := httptest.NewRecorder()
		Handler(Config{}, Options{Store: books, Kernel: a}).ServeHTTP(w, httptest.NewRequest("POST", "/v1/dev/bl/u/brief", strings.NewReader(`{"date":"2026-10-28"}`)))
		if w.Code != tc.status || strings.Contains(w.Body.String(), "private-content") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestBriefTailBudget(t *testing.T) {
	segments := make([]domain.PageSegment, 10)
	for i := range segments {
		segments[i] = domain.PageSegment{Speaker: "wearer", Text: strings.Repeat("旧", 2000)}
	}
	segments[9] = domain.PageSegment{Speaker: "other", SpeakerKey: "other-1", SpeakerRef: "P-0012", Text: "最新😀"}
	input := briefInput(segments)
	if !utf8.ValidString(input) || utf8.RuneCountInString(input) != 8000 || !strings.HasPrefix(input, "[佩戴者] ") || !strings.HasSuffix(input, "[他人 other-1 @P-0012] 最新😀") {
		t.Fatal("invalid tail budget")
	}
	segments = make([]domain.PageSegment, 201)
	for i := range segments {
		segments[i] = domain.PageSegment{Speaker: "wearer", Text: "短"}
	}
	segments[0].Text = "应舍弃"
	if got := briefInput(segments); strings.Contains(got, "应舍弃") || len(strings.Split(got, "\n")) != 200 {
		t.Fatal("segment limit")
	}
}

func TestBriefEmptyAndInvalidDay(t *testing.T) {
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"date":"2026-10-28"}`, 409},
		{`{"date":"bad"}`, 400},
		{`{"date":"2026-10-28","input":"不能自带原文"}`, 400},
	} {
		a := &briefActions{call: func(context.Context, string, string, string, string) (kernel.OneshotResult, error) {
			t.Fatal("empty or invalid day called model")
			return kernel.OneshotResult{}, nil
		}}
		w := httptest.NewRecorder()
		Handler(Config{}, Options{Store: memstore.New(), Kernel: a}).ServeHTTP(w, httptest.NewRequest("POST", "/v1/dev/bl/u/brief", strings.NewReader(tc.body)))
		if w.Code != tc.status {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	Handler(Config{}, Options{Store: memstore.New()}).ServeHTTP(w, httptest.NewRequest("POST", "/v1/dev/bl/u/brief", strings.NewReader(`{"date":"2026-10-28"}`)))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}
