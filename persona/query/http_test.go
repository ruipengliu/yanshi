package query_test

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
	"persona/mcpserver"
	"persona/query"
	"persona/store"
	"persona/store/memstore"
)

func TestServiceCredentials(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	books := memstore.New()
	for _, line := range []string{"a", "b"} {
		if err := books.Update(context.Background(), line, "same-user", func(book *domain.Book) error {
			book.Seed(domain.SeedData{Projects: []domain.Project{{ID: "p", Name: line}}}, now)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	handler := query.Handler(&query.Service{Store: books}, []mcpserver.Credential{{BusinessLine: "a", Token: "old", ExpiresAt: now}, {BusinessLine: "a", Token: "current", ExpiresAt: now.Add(time.Hour)}, {BusinessLine: "b", Token: "other", ExpiresAt: now.Add(time.Hour)}}, func() time.Time { return now })
	for _, tc := range []struct {
		token, path string
		status      int
		name        string
	}{
		{"", "/v1/query/same-user/projects/p", 401, ""},
		{"old", "/v1/query/same-user/projects/p", 401, ""},
		{"current", "/v1/query/same-user/projects/p", 200, "a"},
		{"other", "/v1/query/same-user/projects/p", 200, "b"},
		{"current", "/v1/query/same-user/projects/p?business_line=b", 400, ""},
		{"current", "/v1/query/unknown/projects/p", 404, ""},
		{"current", "/v1/query/same-user/notes", 404, ""},
	} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s: status %d", tc.path, w.Code)
		}
		if tc.name != "" {
			var project domain.Project
			if json.Unmarshal(w.Body.Bytes(), &project) != nil || project.Name != tc.name {
				t.Fatal("credential business line ignored")
			}
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/query/same-user/projects/p", nil)
	r.Header.Set("Authorization", "Bearer current")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Fatal("query accepted write")
	}
}

type barrierStore struct{ store.Store }

func (s barrierStore) View(ctx context.Context, line, user string, fn func(*domain.Book) error) error {
	if err := s.Store.View(ctx, line, user, fn); err != nil {
		return err
	}
	return store.ErrOwnerDeleting
}

func TestViewFailureDiscardsPayload(t *testing.T) {
	books := memstore.New()
	if err := books.Update(context.Background(), "bl", "u", func(book *domain.Book) error {
		book.Seed(domain.SeedData{Projects: []domain.Project{{ID: "p", Name: "secret"}}}, time.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := query.Service{Store: barrierStore{books}}
	data, err := svc.Read(context.Background(), store.Owner{BusinessLine: "bl", EndUser: "u"}, query.Request{Resource: "projects", Limit: 1})
	if !errors.Is(err, store.ErrOwnerDeleting) || len(data) != 0 {
		t.Fatal("failed View returned payload")
	}
}

func TestClientSanitizesFailuresAndRedirects(t *testing.T) {
	for _, status := range []int{301, 401, 500, 503} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/other")
			w.WriteHeader(status)
			_, _ = w.Write([]byte("private request body"))
		}))
		client := query.Client{BaseURL: upstream.URL, Token: func(context.Context, string) (string, error) { return "service", nil }}
		_, err := client.Read(context.Background(), store.Owner{BusinessLine: "bl", EndUser: "u"}, query.Request{Resource: "projects", Limit: 1})
		upstream.Close()
		if err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("unsafe upstream error")
		}
	}
}
