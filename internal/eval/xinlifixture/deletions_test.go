package xinlifixture

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 只验证线上契约，不依赖 Persona 的 Go 模块或真实内核客户端。
func TestDeletionClient(t *testing.T) {
	date := "2026-10-27"
	deadline := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var in map[string]string
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Error(err)
			}
			if in["date"] != date {
				t.Error("business date missing")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		task := DeletionTask{ID: "del_2026-10-27_1", Kind: "erase", Date: date, Session: "day", Sessions: []string{"old", "day"}, Status: "pending", DeleteAt: deadline}
		switch r.URL.Path {
		case "/v1/fixture/bl/u/bind_day":
			if in["delete_at"] != deadline.Format(time.RFC3339Nano) || in["session"] != "day" {
				t.Error("bind contract")
			}
			_ = json.NewEncoder(w).Encode(struct{}{})
		case "/v1/fixture/bl/u/erase":
			if in["from"] != "1.0" || in["to"] != "1.1" {
				t.Error("range missing")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"task": task})
		case "/v1/fixture/bl/u/erase_day":
			task.Kind = "day"
			_ = json.NewEncoder(w).Encode(map[string]any{"task": task})
		case "/v1/fixture/bl/u/deletion_tasks":
			if r.Method != http.MethodGet {
				t.Error("query must be GET")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tasks": []DeletionTask{task}})
		default:
			t.Error("unexpected endpoint", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, http: srv.Client()}
	ctx := context.Background()
	if err := c.BindDay(ctx, "bl", "u", date, "day", deadline); err != nil {
		t.Fatal(err)
	}
	task, err := c.Erase(ctx, "bl", "u", date, "1.0", "1.1")
	if err != nil || task.Kind != "erase" || len(task.Sessions) != 2 || !task.DeleteAt.Equal(deadline) {
		t.Fatal(task, err)
	}
	task, err = c.EraseDay(ctx, "bl", "u", date)
	if err != nil || task.Kind != "day" {
		t.Fatal(task, err)
	}
	tasks, err := c.DeletionTasks(ctx, "bl", "u")
	if err != nil || len(tasks) != 1 || tasks[0].ID != task.ID || calls != 4 {
		t.Fatal(tasks, err, calls)
	}
}
