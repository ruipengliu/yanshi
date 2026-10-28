package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestScheduleHTTP(t *testing.T) {
	srv, _ := setup(t)
	var created struct {
		SessionID string `json:"session_id"`
	}
	if code := post(t, srv.URL+"/v1/sessions", map[string]string{"business_line": "demo", "end_user": "u", "agent": "echo", "time_zone": "Asia/Shanghai"}, &created); code != http.StatusCreated {
		t.Fatalf("create session: %d", code)
	}
	base := srv.URL + "/v1/sessions/" + created.SessionID
	resp, err := http.Get(base)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if state["time_zone"] != "Asia/Shanghai" {
		t.Fatalf("zone: %v", state)
	}
	var item map[string]any
	body := map[string]string{"schedule_id": "one", "input": "提醒", "rrule": "FREQ=DAILY;BYHOUR=9;BYMINUTE=0"}
	if code := post(t, base+"/schedules", body, &item); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, item)
	}
	if item["next_due"] == nil || item["next_due_local"] == "" || item["time_zone"] != "Asia/Shanghai" {
		t.Fatalf("schedule: %v", item)
	}
	if code := post(t, base+"/schedules", body, nil); code != http.StatusCreated {
		t.Fatalf("retry: %d", code)
	}
	body["input"] = "不同"
	if code := post(t, base+"/schedules", body, nil); code != http.StatusConflict {
		t.Fatalf("conflict: %d", code)
	}
	resp, err = http.Get(base + "/schedules")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Schedules []json.RawMessage `json:"schedules"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(list.Schedules) != 1 {
		t.Fatalf("list: %v", list)
	}
	for _, want := range []int{http.StatusNoContent, http.StatusNotFound} {
		req, _ := http.NewRequest(http.MethodDelete, base+"/schedules/one", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("delete: %d want %d", resp.StatusCode, want)
		}
	}
	if code := post(t, base+"/inputs", map[string]any{"text": "伪造", "schedule": map[string]string{"schedule_id": "one"}}, nil); code != http.StatusBadRequest {
		t.Fatalf("client schedule mark: %d", code)
	}
	if code := post(t, srv.URL+"/v1/sessions", map[string]string{"business_line": "demo", "end_user": "u", "agent": "echo", "time_zone": "bad/zone"}, nil); code != http.StatusBadRequest {
		t.Fatalf("invalid zone: %d", code)
	}
}
