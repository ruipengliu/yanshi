package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDeadlineAuthorizationAndValidation(t *testing.T) {
	e := newAuthzEnv(t)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		who, value string
		want       int
	}{
		{"demo/u1", `"` + future + `"`, 403}, {"demo/u1", `null`, 403},
		{"demo/", `"` + future + `"`, 201}, {"demo/", `"2020-01-01T00:00:00Z"`, 400},
		{"demo/", `"bad"`, 400}, {"demo/", `null`, 400}, {"demo/", `"0001-01-01T00:00:00Z"`, 400},
	} {
		code, body := e.do(t, "POST", "/v1/sessions", e.token(t, tc.who, time.Hour), `{"agent":"echo","end_user":"u1","delete_at":`+tc.value+`}`)
		if code != tc.want {
			t.Fatalf("%s %s: %d %s", tc.who, tc.value, code, body)
		}
	}
}

func TestErasureProgressAndCandidates(t *testing.T) {
	e := newAuthzEnv(t)
	token := e.token(t, "demo/", time.Hour)
	path := "/v1/sessions/" + e.sid
	for _, tc := range []struct {
		who, suffix string
		want        int
	}{
		{"demo/u1", "?reason=erasure", 403}, {"other/", "?reason=erasure", 404},
		{"demo/", "?reason=retention", 400}, {"demo/", "?reason=", 400}, {"demo/", "?reason=erasure&reason=erasure", 400},
	} {
		code, body := e.do(t, "DELETE", path+tc.suffix, e.token(t, tc.who, time.Hour), nil)
		if code != tc.want {
			t.Fatalf("delete: %d %s", code, body)
		}
	}
	if code, _ := e.do(t, "GET", path+"/deletion", token, nil); code != 404 {
		t.Fatalf("no tombstone = %d", code)
	}
	if code, body := e.do(t, "DELETE", path+"?reason=erasure", token, nil); code != 202 {
		t.Fatalf("delete: %d %s", code, body)
	}
	if code, _ := e.do(t, "GET", path, token, nil); code != 404 {
		t.Fatalf("deleted session visible: %d", code)
	}
	for _, completed := range []bool{false, true} {
		if completed {
			if err := e.api.Service.Store.Log.Delete(context.Background(), e.sid); err != nil {
				t.Fatal(err)
			}
			if err := e.api.Service.Deletions.Complete(context.Background(), e.sid, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		code, body := e.do(t, "GET", path+"/deletion", token, nil)
		var progress map[string]any
		if err := json.Unmarshal([]byte(body), &progress); err != nil {
			t.Fatal(err)
		}
		if code != 200 || progress["reason"] != "erasure" || progress["requested_at"] == nil || (progress["completed_at"] != nil) != completed {
			t.Fatalf("progress: %d %s", code, body)
		}
	}
	query := "/v1/memories?end_user=u1&source_session=" + e.sid + "&source_session=missing"
	for _, tc := range []struct {
		who     string
		want    int
		visible bool
	}{
		{"demo/", 200, true}, {"other/", 200, false}, {"demo/u1", 403, false},
	} {
		code, body := e.do(t, "GET", query, e.token(t, tc.who, time.Hour), nil)
		if code != tc.want || strings.Contains(body, e.memory) != tc.visible {
			t.Fatalf("candidates: %d %s", code, body)
		}
	}
	for _, query := range []string{
		"source_session=", "source_session=s&history=true", "source_session=s&limit=1", "source_session=s&q=x", "source_session=s&category=profile", strings.Repeat("source_session=s&", 33),
	} {
		code, body := e.do(t, "GET", "/v1/memories?end_user=u1&"+query, token, nil)
		if code != http.StatusBadRequest {
			t.Fatalf("invalid candidates: %d %s", code, body)
		}
	}
	if code, _ := e.do(t, "GET", "/v1/memories?source_session=s", token, nil); code != 400 {
		t.Fatalf("missing end_user: %d", code)
	}
}
