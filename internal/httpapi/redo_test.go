package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
	"yanshi/internal/service"
)

func httpRedoFixture(t *testing.T) (*authzEnv, string, string) {
	t.Helper()
	e := newAuthzEnv(t)
	s := e.api.Service
	s.Quotas = nil
	sid, err := s.Create(t.Context(), service.CreateRequest{BusinessLine: "demo", EndUser: "u1", Agent: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Submit(t.Context(), sid, model.TextBlocks("问题"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Load(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Commit(t.Context(), st,
		&v1.Event{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: res.RunID, Attempt: 1}}},
		&v1.Event{Payload: &v1.Event_RunCompleted{RunCompleted: &v1.RunCompleted{RunId: res.RunID, Attempt: 1}}}); err != nil {
		t.Fatal(err)
	}
	return e, sid, res.RunID
}

func TestHTTPRedoValidationAndConflict(t *testing.T) {
	e, sid, run := httpRedoFixture(t)
	token := e.token(t, "demo/u1", time.Hour)
	path := "/v1/sessions/" + sid + "/inputs"
	for _, body := range []string{
		`{"redo":true}`,
		`{"redo":true,"expected_run_id":""}`,
		`{"redo":true,"expected_run_id":"r","text":""}`,
		`{"redo":true,"expected_run_id":"r","content":[]}`,
		`{"redo":true,"expected_run_id":"r","content":null}`,
		`{"redo":true,"expected_run_id":"r","input":"hi"}`,
		`{"redo":true,"expected_run_id":"r","schedule":null}`,
		`{"text":"hi","redo_of":"r"}`,
		`{"text":"hi","redo_of":null}`,
		`{"redo":true,"expected_run_id":"r","redo_of":"r"}`,
		`{"redo":true,"expected_run_id":"r","input_id":"schedule_forged"}`,
		`{"text":"hi","expected_run_id":"r"}`,
		fmt.Sprintf(`{"redo":true,"expected_run_id":%q,"input_id":%q}`, run, strings.Repeat("x", service.MaxInputIDLen+1)),
	} {
		if code, reply := e.do(t, http.MethodPost, path, token, body); code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", body, code, reply)
		}
	}
	body := fmt.Sprintf(`{"redo":true,"input_id":"redo","expected_run_id":%q}`, run)
	code, reply := e.do(t, http.MethodPost, path, token, body)
	if code != http.StatusAccepted {
		t.Fatalf("redo: %d %s", code, reply)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(reply), &first); err != nil {
		t.Fatal(err)
	}
	code, reply = e.do(t, http.MethodPost, path, token, body)
	var again map[string]any
	if err := json.Unmarshal([]byte(reply), &again); err != nil {
		t.Fatal(err)
	}
	if code != http.StatusAccepted || again["duplicate"] != true || again["run_id"] != first["run_id"] || again["steered"] != false {
		t.Fatalf("duplicate: %d %s", code, reply)
	}
	code, reply = e.do(t, http.MethodPost, path, token, fmt.Sprintf(`{"redo":true,"input_id":"other-device","expected_run_id":%q}`, run))
	if code != http.StatusConflict || !strings.Contains(reply, "not_last_run") {
		t.Fatalf("stale expected run: %d %s", code, reply)
	}
}

func TestHTTPRedoAuthorization(t *testing.T) {
	for principal, want := range map[string]int{
		"demo/u1":  http.StatusAccepted,
		"demo/":    http.StatusAccepted,
		"demo/u2":  http.StatusNotFound,
		"other/u1": http.StatusNotFound,
		"other/":   http.StatusNotFound,
		"":         http.StatusUnauthorized,
	} {
		t.Run(principal, func(t *testing.T) {
			e, sid, run := httpRedoFixture(t)
			code, body := e.do(t, http.MethodPost, "/v1/sessions/"+sid+"/inputs", e.token(t, principal, time.Hour),
				fmt.Sprintf(`{"redo":true,"input_id":"redo","expected_run_id":%q}`, run))
			if code != want {
				t.Fatalf("got %d %s, want %d", code, body, want)
			}
		})
	}
}
