package account_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"xinli/account"
	"xinli/task"
)

type unavailableAuthStore struct {
	account.Store
	phase string
}

func (s unavailableAuthStore) TokenOwner(ctx context.Context, token string) (task.Owner, error) {
	if s.phase == "token" {
		return task.Owner{}, errors.New("private storage detail")
	}
	return s.Store.TokenOwner(ctx, token)
}
func (s unavailableAuthStore) ReadAccount(ctx context.Context, owner task.Owner) (account.Record, error) {
	if s.phase == "account" {
		return account.Record{}, errors.New("private storage detail")
	}
	return s.Store.ReadAccount(ctx, owner)
}
func TestAuthenticationStorageFailureIsRetryable(t *testing.T) {
	for _, phase := range []string{"token", "account"} {
		t.Run(phase, func(t *testing.T) {
			backend := setup(t, "memory")
			now := time.Now()
			s := service(backend, &now)
			credentials := login(t, s, "phone")
			s.Store = unavailableAuthStore{Store: s.Store, phase: phase}
			called := false
			handler := s.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			if resp.Code != http.StatusServiceUnavailable || resp.Header().Get("Retry-After") != "1" || called || strings.Contains(resp.Body.String(), "private") {
				t.Fatalf("phase=%s status=%d called=%t", phase, resp.Code, called)
			}
		})
	}
}
