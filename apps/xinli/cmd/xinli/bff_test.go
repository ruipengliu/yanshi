package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"persona/domain"
	"persona/mcpserver"
	"persona/query"
	"persona/store/memstore"
	"xinli/account"
	"xinli/kernel"
	taskmem "xinli/task/memstore"
)

func TestBFFRoleQueries(t *testing.T) {
	books := memstore.New()
	tasks := taskmem.New()
	accounts := &account.Service{Store: tasks, Provider: account.TestProvider{Identities: []account.TestIdentity{{BusinessLine: "bl", Subject: "user", Code: "proof"}}}}
	credentials, err := accounts.Login(context.Background(), "bl", "proof", "phone", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := books.Update(context.Background(), "bl", credentials.Owner.EndUser, func(book *domain.Book) error {
		book.Seed(domain.SeedData{Projects: []domain.Project{{ID: "p", Name: "project"}}}, time.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	serviceCredentials := []mcpserver.Credential{{BusinessLine: "bl", Token: "service", ExpiresAt: time.Now().Add(time.Hour)}}
	persona := httptest.NewServer(query.Handler(&query.Service{Store: books}, serviceCredentials, nil))
	defer persona.Close()
	for _, remote := range []string{"", persona.URL} {
		cfg := config{mode: "serve", roles: roles{bff: true}, personaURL: remote, credentials: serviceCredentials}
		handler := routes(cfg, books, nil, &kernel.Client{}, accounts)
		r := httptest.NewRequest(http.MethodGet, "/v1/projects/p", nil)
		r.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var project domain.Project
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &project) != nil || project.ID != "p" {
			t.Fatalf("BFF mount: %d", w.Code)
		}
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/projects", nil))
		if w.Code != 401 {
			t.Fatal("BFF missing authentication")
		}
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/query/u/projects", nil))
		if w.Code != 404 {
			t.Fatal("BFF role exposed Persona query endpoint")
		}
	}
	cfg := config{mode: "serve", roles: roles{persona: true}, credentials: serviceCredentials}
	w := httptest.NewRecorder()
	routes(cfg, books, nil, nil).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/query/u/projects", nil))
	if w.Code != 401 {
		t.Fatal("Persona query mount missing service authentication")
	}
}

func TestBFFRequiresKernelTokenSource(t *testing.T) {
	t.Setenv("YANSHI_PG_DSN", "")
	args := productionArgs(t, "postgres://localhost/persona", "postgres://localhost/xinli", "http://localhost:8080")
	cfg, err := parseConfig(append(args, "-roles", "bff", "-persona-url", "http://localhost:8081"), io.Discard)
	if err != nil || cfg.tokens == nil || len(cfg.credentials) == 0 {
		t.Fatalf("BFF configuration: %v", err)
	}
	if _, err := parseConfig(append(args, "-roles", "bff", "-kernel-url", ""), io.Discard); err == nil {
		t.Fatal("BFF accepted missing kernel URL")
	}
}
