package devui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
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
			books := memstore.New(captureClock)
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
						_, err := b.Erase("2026-10-28", "1.0", "1.0", captureClock())
						return err
					case "erase_day":
						_, err := b.EraseDay("2026-10-28", captureClock())
						return err
					case "append":
						_, err := b.AppendCaptures("2026-10-28", "next", []domain.CaptureSegment{{Speaker: "wearer", Text: "新增"}})
						return err
					}
					return nil
				})
				return kernel.OneshotResult{Output: json.RawMessage(`{"brief":"私密简述"}`)}, nil
			}}
			h := Handler(Config{}, Options{Store: books, Kernel: a, Now: captureClock})
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
		books := memstore.New(captureClock)
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
		Handler(Config{}, Options{Store: books, Kernel: a, Now: captureClock}).ServeHTTP(w, httptest.NewRequest("POST", "/v1/dev/bl/u/brief", strings.NewReader(`{"date":"2026-10-28"}`)))
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
		Handler(Config{}, Options{Store: memstore.New(captureClock), Kernel: a}).ServeHTTP(w, httptest.NewRequest("POST", "/v1/dev/bl/u/brief", strings.NewReader(tc.body)))
		if w.Code != tc.status {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	Handler(Config{}, Options{Store: memstore.New(captureClock)}).ServeHTTP(w, httptest.NewRequest("POST", "/v1/dev/bl/u/brief", strings.NewReader(`{"date":"2026-10-28"}`)))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func TestBriefVisibleProjectionRoutes(t *testing.T) {
	for _, allHidden := range []bool{false, true} {
		t.Run(fmt.Sprint(allHidden), func(t *testing.T) {
			books := memstore.New(captureClock)
			if err := books.Update(context.Background(), "bl", "u", func(b *domain.Book) error {
				if !allHidden {
					_, err := b.AppendCaptures("2026-10-28", "visible", []domain.CaptureSegment{
						{Speaker: "wearer", Text: "证据"},
						{Speaker: "wearer", SpeakerGrade: "timeline", Text: "时间线"},
						{Speaker: "unknown", Text: "未知"},
						{Speaker: "other", SpeakerKey: "o", SpeakerRef: "P-1", Text: "他人"},
					})
					if err != nil {
						return err
					}
				}
				// 隐藏尾批不得挤掉简述的 200 段可见预算。
				hidden := make([]domain.CaptureSegment, 200)
				for i := range hidden {
					hidden[i] = domain.CaptureSegment{Speaker: "unknown", Text: "不应交给模型", Background: true}
				}
				_, err := b.AppendCaptures("2026-10-28", "hidden", hidden)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			a := &briefActions{call: func(_ context.Context, _, _, _, input string) (kernel.OneshotResult, error) {
				calls++
				if allHidden || input != "[佩戴者] 证据\n[他人 u-1-1] 时间线\n[他人 u-1-2] 未知\n[他人 o @P-1] 他人" {
					t.Fatalf("untrusted brief input: %q", input)
				}
				return kernel.OneshotResult{Output: json.RawMessage(`{"brief":"简述"}`)}, nil
			}}
			h := Handler(Config{}, Options{Store: books, Kernel: a, Now: captureClock})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/dev/bl/u/brief", strings.NewReader(`{"date":"2026-10-28"}`)))
			if allHidden && (w.Code != 409 || calls != 0) || !allHidden && (w.Code != 200 || calls != 1) {
				t.Fatal(w.Code, calls, w.Body.String())
			}
			w = httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/dev/bl/u/captures?date=2026-10-28", nil))
			var out struct {
				Segments []domain.CaptureView `json:"segments"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Segments) < 200 || !out.Segments[len(out.Segments)-1].Background {
				t.Fatal("dev view hid raw segments", w.Code)
			}
			if !allHidden && (out.Segments[1].Speaker != "wearer" || out.Segments[1].SpeakerGrade != "timeline" || out.Segments[2].Speaker != "unknown") {
				t.Fatal("dev view projected raw identity")
			}
		})
	}
}
