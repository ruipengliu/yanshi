package fixture_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"persona/fixture"
	"persona/store"
)

func TestOwnerDeletion(t *testing.T) {
	fixtureStores(t, func(t *testing.T, s store.Store) {
		h := fixture.Handler(s, fixture.Options{Now: captureClock})
		request := func(method, path, body string) *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			h.ServeHTTP(r, httptest.NewRequest(method, "/v1/fixture/bl/u/"+path, strings.NewReader(body)))
			return r
		}
		if r := request("GET", "owner_deletion", ""); r.Code != 404 {
			t.Fatal(r.Code, r.Body.String())
		}
		for range 2 {
			r := request("POST", "owner_deletion", "{}")
			var out store.OwnerDeletion
			if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil || r.Code != 200 || out.Status != "pending" || !out.StartedAt.Equal(captureClock()) {
				t.Fatal(r.Code, out, err)
			}
		}
		for _, call := range [][3]string{
			{"GET", "state", ""}, {"GET", "deletion_tasks", ""}, {"POST", "seed", "{}"},
			{"POST", "reset", "{}"}, {"POST", "captures", "{}"}, {"POST", "erase_day", "{}"},
			{"POST", "edit", "{}"}, {"POST", "capture_preview", "{}"},
		} {
			r := request(call[0], call[1], call[2])
			if r.Code != 400 || !strings.Contains(r.Body.String(), store.ErrOwnerDeleting.Error()) {
				t.Fatal("fixture bypassed barrier", call[1], r.Code, r.Body.String())
			}
		}
		if err := (store.Purger{Store: s, Now: captureClock}).Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
		if r := request("GET", "owner_deletion", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"status":"completed"`) {
			t.Fatal(r.Code, r.Body.String())
		}
	})
}
