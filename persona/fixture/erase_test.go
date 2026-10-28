package fixture_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"persona/domain"
	"persona/fixture"
	"persona/store"
	"persona/store/memstore"
)

type deletionNotice func(context.Context, string, string, string)

func (f deletionNotice) DeletionPending(ctx context.Context, bl, eu, id string) { f(ctx, bl, eu, id) }

// 通知内读取存储，证明已提交且锁已释放；失败事务不应触发通知。
func TestErasureRoutes(t *testing.T) {
	s := memstore.New()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	var notices []string
	notifier := deletionNotice(func(ctx context.Context, bl, eu, id string) {
		if bl != "bl" || eu != "u" {
			t.Fatal("notification owner")
		}
		if err := s.View(ctx, bl, eu, func(b *domain.Book) error {
			for _, task := range b.DeletionTasks() {
				if task.ID == id {
					notices = append(notices, id)
					return nil
				}
			}
			t.Fatal("notification before commit")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	h := fixture.Handler(s, fixture.Options{Now: func() time.Time { return now }, Notifier: notifier})
	request := func(method, path, body string, code int) []byte {
		t.Helper()
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(method, "/v1/fixture/bl/u/"+path, strings.NewReader(body)))
		if r.Code != code {
			t.Fatalf("%s: HTTP %d: %s", path, r.Code, r.Body.String())
		}
		return r.Body.Bytes()
	}
	if got := string(request("GET", "deletion_tasks", "", 200)); got != "{\"tasks\":[]}\n" {
		t.Fatal(got)
	}
	request("POST", "captures", `{"date":"2026-10-27","capture_id":"a","segments":[{"speaker":"wearer","text":"擦去"},{"speaker":"wearer","text":"留下"}]}`, 200)
	request("POST", "bind_day", `{"date":"2026-10-27","session":"s","delete_at":"2026-11-03T12:00:00Z"}`, 200)
	request("POST", "bind_day", `{"date":"2026-10-27","session":"s","delete_at":"bad"}`, 400)
	request("POST", "erase", `{"date":"2026-10-27","from":"1.0","to":"9.0"}`, 400)
	if len(notices) != 0 {
		t.Fatal("notified invalid erasure")
	}
	var result struct {
		Task domain.DeletionTask `json:"task"`
	}
	if err := json.Unmarshal(request("POST", "erase", `{"date":"2026-10-27","from":"1.0","to":"1.0"}`, 200), &result); err != nil {
		t.Fatal(err)
	}
	if result.Task.ID != "del_2026-10-27_1" || result.Task.Session != "s" || result.Task.Status != "pending" || result.Task.DeleteAt.Format(time.RFC3339) != "2026-11-03T12:00:00Z" || len(notices) != 1 {
		t.Fatal(result)
	}
	request("POST", "capture_preview", `{"session":"s","page_options":{"limit":200,"max_chars":6000,"gap_ms":300000}}`, 400)
	request("POST", "bind_day", `{"date":"2026-10-27","session":"new"}`, 200)
	raw := request("POST", "capture_preview", `{"session":"new","page_options":{"limit":200,"max_chars":6000,"gap_ms":300000}}`, 200)
	if strings.Contains(string(raw), "擦去") || !strings.Contains(string(raw), "留下") {
		t.Fatal("wrong remaining page")
	}
	if err := json.Unmarshal(request("POST", "erase_day", `{"date":"2026-10-27"}`, 200), &result); err != nil {
		t.Fatal(err)
	}
	if result.Task.ID != "del_2026-10-27_2" || result.Task.Kind != "day" || len(result.Task.Sessions) != 2 || len(notices) != 2 {
		t.Fatal(result)
	}
	request("POST", "bind_day", `{"date":"2026-10-27","session":"again"}`, 400)
	request("POST", "captures", `{"date":"2026-10-27","capture_id":"b","segments":[{"speaker":"wearer","text":"不能重现"}]}`, 400)
	var tasks struct {
		Tasks []domain.DeletionTask `json:"tasks"`
	}
	if err := json.Unmarshal(request("GET", "deletion_tasks", "", 200), &tasks); err != nil || len(tasks.Tasks) != 2 {
		t.Fatal(err, tasks)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(request("GET", "state", "", 200), &state); err != nil {
		t.Fatal(err)
	}
	if _, ok := state["deletion_tasks"]; ok || len(state) != 8 {
		t.Fatal("State shape changed")
	}
	request("POST", "deletion_tasks", `{}`, 405)
	request("GET", "erase", "", 405)
	for _, op := range []string{"erase", "erase_day"} {
		request("POST", op, `{"unexpected":true}`, 400)
	}
}

type rollbackDeletionStore struct{ store.Store }

func (s rollbackDeletionStore) Update(ctx context.Context, bl, eu string, fn func(*domain.Book) error) error {
	return s.Store.Update(ctx, bl, eu, func(b *domain.Book) error {
		if err := fn(b); err != nil {
			return err
		}
		return errors.New("transaction failed")
	})
}

func TestErasureNotificationRollback(t *testing.T) {
	s := memstore.New()
	if err := s.Update(context.Background(), "bl", "u", func(b *domain.Book) error {
		return b.BindDay("2026-10-27", "s")
	}); err != nil {
		t.Fatal(err)
	}
	h := fixture.Handler(rollbackDeletionStore{s}, fixture.Options{Notifier: deletionNotice(func(context.Context, string, string, string) { t.Fatal("notified rollback") })})
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("POST", "/v1/fixture/bl/u/erase_day", strings.NewReader(`{"date":"2026-10-27"}`)))
	if r.Code != 400 {
		t.Fatal(r.Code)
	}
	if owners, err := s.PendingDeletions(context.Background()); err != nil || len(owners) != 0 {
		t.Fatal(owners, err)
	}
}
