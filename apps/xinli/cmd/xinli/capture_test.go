package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	personaMem "persona/store/memstore"
	"xinli/account"
	"xinli/task/memstore"
)

func TestCaptureRouteRequiresApplicationSession(t *testing.T) {
	accounts := &account.Service{Store: memstore.New()}
	for _, mode := range []string{"serve", "dev"} {
		handler := routes(config{mode: mode, roles: roles{bff: true}}, personaMem.New(), nil, nil, accounts)
		request := httptest.NewRequest(http.MethodPost, "/v1/captures/batches", strings.NewReader(`{}`))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatal(mode, response.Code)
		}
	}
}
