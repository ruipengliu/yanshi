package command_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"persona/command"
	"persona/domain"
	"persona/mcpserver"
	"persona/pg/pgtest"
	"persona/query"
	"persona/store"
	"persona/store/memstore"
	"persona/store/pgstore"
)

func TestCommandsConcurrentAndCredentials(t *testing.T) {
	for _, kind := range []string{"memory", "pg"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
			clock := func() time.Time { return now }
			var books store.Store = memstore.New(clock)
			if kind == "pg" {
				books = pgstore.New(pgtest.New(t), clock)
			}
			ctx := context.Background()
			for _, line := range []string{"a", "b"} {
				if err := books.Update(ctx, line, "user", func(b *domain.Book) error {
					b.Seed(domain.SeedData{Commitments: []domain.SeedCommitment{{ID: "c", Kind: "mine", Content: line}}, Projects: []domain.Project{{ID: "p", Name: line}}}, now)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			svc := &command.Service{Store: books, Now: clock}
			handler := command.Handler(svc, []mcpserver.Credential{{BusinessLine: "a", Token: "expired", ExpiresAt: now}, {BusinessLine: "a", Token: "current", ExpiresAt: now.Add(time.Hour)}}, clock)
			for _, tc := range []struct {
				token, body string
				status      int
			}{
				{"", `{"id":"c","rev":1,"status":"done"}`, 401},
				{"expired", `{"id":"c","rev":1,"status":"done"}`, 401},
				{"current", `{"id":"c","rev":1,"status":"done","author":"user"}`, 400},
				{"current", `{"id":"c","rev":1,"status":"done","business_line":"b"}`, 400},
				{"current", `{"id":"c","rev":null,"status":"done"}`, 400},
				{"current", `{"id":"c","rev":0,"status":"done"}`, 400},
			} {
				r := httptest.NewRequest("POST", "/v1/commands/user/commitment", strings.NewReader(tc.body))
				r.Header.Set("Authorization", "Bearer "+tc.token)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != tc.status {
					t.Fatalf("status %d want %d", w.Code, tc.status)
				}
			}
			server := httptest.NewServer(handler)
			defer server.Close()
			client := &command.Client{BaseURL: server.URL, Token: func(context.Context, string) (string, error) { return "current", nil }}
			owner := store.Owner{BusinessLine: "a", EndUser: "user"}
			results := make(chan error, 2)
			for _, status := range []string{"done", "cancelled"} {
				go func() {
					_, err := client.Execute(ctx, owner, command.Request{Operation: "commitment", ID: "c", Rev: json.RawMessage("1"), Status: status})
					results <- err
				}()
			}
			success, conflict := 0, 0
			for range 2 {
				err := <-results
				var remote *query.Error
				switch {
				case err == nil:
					success++
				case errors.As(err, &remote) && remote.Status == 409:
					conflict++
				default:
					t.Fatalf("unexpected command error: %v", err)
				}
			}
			if success != 1 || conflict != 1 {
				t.Fatalf("success=%d conflict=%d", success, conflict)
			}
			if err := books.View(ctx, "b", "user", func(b *domain.Book) error {
				m, err := b.QueryCommitment("c")
				if m.Rev() != 1 || m.Status != "open" {
					t.Error("credential crossed business line")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := books.BeginOwnerDeletion(ctx, "a", "user", now); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Execute(ctx, owner, command.Request{Operation: "commitment", ID: "c", Rev: json.RawMessage("2"), Status: "done"}); err == nil {
				t.Fatal("command bypassed owner deletion barrier")
			}
		})
	}
}

func TestCommandClientSanitizesErrorsAndRejectsRedirect(t *testing.T) {
	for _, status := range []int{409, 500, 302} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/private")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"private downstream content"}`))
		}))
		client := command.Client{BaseURL: server.URL, Token: func(context.Context, string) (string, error) { return "service-secret", nil }}
		_, err := client.Execute(context.Background(), store.Owner{BusinessLine: "a", EndUser: "u"}, command.Request{Operation: "commitment"})
		server.Close()
		if err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "service-secret") {
			t.Fatal("unsafe remote error")
		}
	}
}
