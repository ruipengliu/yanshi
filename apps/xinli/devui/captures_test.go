package devui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"persona/domain"
	"persona/store/memstore"
)

type devActions struct {
	dates, texts, notifications []string
	fail                        bool
}

func (d *devActions) EnsureDaySession(_ context.Context, _, _, date string) (string, error) {
	d.dates = append(d.dates, date)
	if d.fail {
		return "", errors.New("private-content")
	}
	return "session", nil
}
func (d *devActions) Submit(_ context.Context, _, _, text string) error {
	d.texts = append(d.texts, text)
	return nil
}
func (d *devActions) DeletionPending(_ context.Context, bl, eu, task string) {
	d.notifications = append(d.notifications, bl+"/"+eu+"/"+task)
}

func TestCaptureRoutes(t *testing.T) {
	books := memstore.New()
	actions := &devActions{}
	h := Handler(Config{}, Options{Store: books, DaySessions: actions, Kernel: actions, Notifier: actions, Now: func() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) }})
	request := func(method, path, body string, status int) []byte {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		if w.Code != status {
			t.Fatalf("%s %s: status=%d body=%s", method, path, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	prefix := "/v1/dev/bl/u"
	body := request("POST", prefix+"/captures", `{"date":"2026-10-28","segments":[{"speaker":"wearer","text":"本人原话"},{"speaker":"other","speaker_key":"a","text":"他人原话"}]}`, 200)
	var result struct {
		Refs []string `json:"refs"`
	}
	if err := json.Unmarshal(body, &result); err != nil || len(result.Refs) != 2 || result.Refs[0] != "1.0" {
		t.Fatalf("refs: %s %v", body, err)
	}
	request("POST", prefix+"/captures", `{"date":"2026-10-28","segments":[{"speaker":"wearer","text":"新的批次"}]}`, 200)
	var page struct {
		Day      domain.BusinessDay    `json:"day"`
		Segments []domain.PageSegment  `json:"segments"`
		Tasks    []domain.DeletionTask `json:"tasks"`
	}
	body = request("GET", prefix+"/captures?date=2026-10-28", "", 200)
	if err := json.Unmarshal(body, &page); err != nil || len(page.Segments) != 3 || page.Segments[2].Ref != "2.0" {
		t.Fatalf("page: %s %v", body, err)
	}
	// 读原文不能冒充模型已经读页。
	if err := books.View(context.Background(), "bl", "u", func(b *domain.Book) error {
		if b.CaptureProgress("2026-10-28") != "0" {
			t.Error("read advanced progress")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"exchange", "day"} {
		request("POST", prefix+"/organize", `{"date":"2026-10-28","kind":"`+kind+`","input_id":"in_`+kind+`"}`, 200)
	}
	if len(actions.texts) != 2 || actions.texts[0] != "整理交谈" || actions.texts[1] != "整理今天" {
		t.Fatalf("texts: %v", actions.texts)
	}
	request("POST", prefix+"/erase", `{"date":"2026-10-28","from":"#1.0","to":"#1.0"}`, 200)
	body = request("GET", prefix+"/captures?date=2026-10-28", "", 200)
	if err := json.Unmarshal(body, &page); err != nil || len(page.Segments) != 2 || len(page.Tasks) != 1 || page.Tasks[0].Status != "pending" {
		t.Fatalf("erase: %s %v", body, err)
	}
	if strings.Contains(string(body), "本人原话") || len(actions.notifications) != 1 {
		t.Fatal("erase or notify failed")
	}
	request("POST", prefix+"/erase_day", `{"date":"2026-10-28"}`, 200)
	body = request("GET", prefix+"/captures?date=2026-10-28", "", 200)
	if err := json.Unmarshal(body, &page); err != nil || len(page.Segments) != 0 || !page.Day.Deleted || len(page.Tasks) != 2 {
		t.Fatalf("day: %s %v", body, err)
	}
	request("POST", prefix+"/captures", `{"date":"2026-10-28","segments":[{"speaker":"wearer","text":"不能复活"}]}`, 400)
	request("GET", "/v1/dev/other/u/captures?date=2026-10-28", "", 200)
	request("GET", prefix+"/captures?date=bad", "", 400)
	request("POST", prefix+"/captures", `{} {}`, 400)
	request("POST", prefix+"/captures", `{"unknown":true}`, 400)
	request("POST", prefix+"/organize", `{"date":"2026-10-28","kind":"bad"}`, 400)
	actions.fail = true
	body = request("POST", prefix+"/organize", `{"date":"2026-10-28","kind":"day"}`, 502)
	if strings.Contains(string(body), "private-content") {
		t.Fatal("leaked downstream error")
	}
	config := request("GET", "/config.json", "", 200)
	if !strings.Contains(string(config), `"orchestration":true`) {
		t.Fatalf("config: %s", config)
	}
}

func TestCaptureRoutesWithoutKernel(t *testing.T) {
	books := memstore.New()
	h := Handler(Config{}, Options{Store: books})
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/v1/dev/bl/u/organize", `{"date":"2026-10-28","kind":"day"}`, 503},
		{"GET", "/config.json", "", 200},
		{"GET", "/captures.js", "", 200},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if w.Code != tc.status {
			t.Fatalf("%s: %d", tc.path, w.Code)
		}
		if tc.path == "/config.json" && !strings.Contains(w.Body.String(), `"orchestration":false`) {
			t.Fatal("enabled without kernel")
		}
	}
	// 没有编排器的删除保持 pending，评测可以自己检查。
	for _, action := range []struct{ path, body string }{
		{"captures", `{"date":"2026-10-28","segments":[{"speaker":"wearer","text":"原文"}]}`},
		{"erase_day", `{"date":"2026-10-28"}`},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/dev/bl/u/"+action.path, strings.NewReader(action.body)))
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	if err := books.View(context.Background(), "bl", "u", func(b *domain.Book) error {
		if len(b.PendingDeletionTasks()) != 1 {
			t.Error("missing pending task")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
