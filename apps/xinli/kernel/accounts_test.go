package kernel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type accountTokens struct{}

func (accountTokens) Token(_ context.Context, line string) (string, error) {
	return "service-" + line, nil
}
func TestAccountHTTPContracts(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		if r.Header.Get("Authorization") != "Bearer service-bl" || r.URL.Query().Get("business_line") != "bl" {
			t.Error("missing service identity")
		}
		switch r.Method + " " + r.URL.Path {
		case "PUT /v1/end_users/user/tier":
			var in map[string]string
			if json.NewDecoder(r.Body).Decode(&in) != nil || in["tier"] != "paid" {
				t.Error("tier request")
			}
			_, _ = w.Write([]byte(`{"tier":"paid","limit_micros":100}`))
		case "DELETE /v1/end_users/user/tier":
			_, _ = w.Write([]byte(`{"tier":"","limit_micros":10}`))
		case "DELETE /v1/end_users/user":
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"request_id":"deletion"}`))
		case "GET /v1/deletions/deletion":
			_, _ = w.Write([]byte(`{"request_id":"deletion","sessions":2,"completed":1,"done":false}`))
		default:
			t.Error("unexpected request")
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	c := &Client{BaseURL: server.URL, Tokens: accountTokens{}}
	tier, err := c.SetTier(context.Background(), "bl", "user", "paid")
	if err != nil || tier.Name != "paid" {
		t.Fatal(tier, err)
	}
	tier, err = c.SetTier(context.Background(), "bl", "user", "")
	if err != nil || tier.Name != "" || tier.LimitMicros != 10 {
		t.Fatal(tier, err)
	}
	id, err := c.DeleteEndUser(context.Background(), "bl", "user")
	if err != nil || id != "deletion" {
		t.Fatal(id, err)
	}
	progress, err := c.EndUserDeletion(context.Background(), "bl", id)
	if err != nil || progress.Done || progress.Completed != 1 {
		t.Fatal(progress, err)
	}
	if count != 4 {
		t.Fatal(count)
	}
}
func TestDeletionProgressFailsClosed(t *testing.T) {
	for _, body := range []string{`{}`, `{"request_id":"d","sessions":0,"completed":0}`, `{"request_id":"other","sessions":0,"completed":0,"done":true}`, `{"request_id":"d","sessions":2,"completed":1,"done":true}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		c := &Client{BaseURL: server.URL}
		if _, err := c.EndUserDeletion(context.Background(), "bl", "d"); err == nil {
			t.Error("invalid progress accepted")
		}
		server.Close()
	}
}
