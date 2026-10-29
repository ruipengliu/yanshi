package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"yanshi/internal/lifecycle"
)

func TestStableSessionList(t *testing.T) {
	e := newAuthzEnv(t)
	ctx := context.Background()
	index := e.api.Service.Index
	for _, row := range []*lifecycle.Session{
		{ID: "a", BusinessLine: "demo", EndUser: "u2"},
		{ID: "b", BusinessLine: "demo", EndUser: "u2"},
		{ID: "c", BusinessLine: "demo", EndUser: "u2"},
		{ID: "foreign", BusinessLine: "other", EndUser: "u2"},
	} {
		row.CreatedAt, row.LastInputAt = time.Now(), time.Now()
		if err := index.Put(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	if err := index.MarkClosed(ctx, "b", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.api.Service.Deletions.Mark(ctx, &lifecycle.Tombstone{SessionID: "c", BusinessLine: "demo", RequestedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	serviceToken := e.token(t, "demo/", time.Hour)
	path := "/v1/sessions?end_user=u2&order=session_id&limit=1"
	cursor := ""
	for _, want := range []string{"a", "b", "c"} {
		code, body := e.do(t, http.MethodGet, path+"&cursor="+url.QueryEscape(cursor), serviceToken, "")
		var page struct {
			Sessions []struct {
				ID     string     `json:"session_id"`
				Closed *time.Time `json:"closed_at"`
			} `json:"sessions"`
			Next string `json:"next_cursor"`
		}
		if code != 200 || json.Unmarshal([]byte(body), &page) != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != want {
			t.Fatalf("page: %d %s", code, body)
		}
		if want == "b" && page.Sessions[0].Closed == nil {
			t.Fatal("closed session missing")
		}
		cursor = page.Next
		if want != "c" && cursor != want || want == "c" && cursor != "" {
			t.Fatalf("cursor: %q", cursor)
		}
		if err := index.Touch(ctx, "b", time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"a", "c"} {
		if code, body := e.do(t, http.MethodGet, "/v1/sessions/"+id+"/events", serviceToken, ""); code != 404 {
			t.Fatalf("empty/deleted index: %d %s", code, body)
		}
	}
	for _, tc := range []struct {
		path, principal string
		want            int
	}{
		{path, "demo/u1", 403},
		{path, "demo/u2", 200},
		{"/v1/sessions?order=session_id", "demo/", 400},
		{"/v1/sessions?order=unknown", "demo/", 400},
		{"/v1/sessions?end_user=u2", "demo/", 200},
	} {
		if code, body := e.do(t, http.MethodGet, tc.path, e.token(t, tc.principal, time.Hour), ""); code != tc.want {
			t.Fatalf("%s %s: %d %s", tc.principal, tc.path, code, body)
		}
	}
	code, body := e.do(t, http.MethodGet, "/v1/sessions?end_user=u2&limit=1", serviceToken, "")
	var recent struct {
		Sessions []struct {
			ID string `json:"session_id"`
		} `json:"sessions"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &recent) != nil || len(recent.Sessions) != 1 || recent.Sessions[0].ID != "b" {
		t.Fatalf("default order changed: %d %s", code, body)
	}
}
