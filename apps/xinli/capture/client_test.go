package capture_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"xinli/capture"
)

func TestResponseStatusIsAuthoritative(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		resync bool
	}{
		{"202_cannot_confirm", 202, `{"status":"accepted"}`, false},
		{"200_requires_acceptance", 200, `{"status":"staged"}`, false},
		{"invalid_json", 200, `{`, false},
		{"resync", 409, `{"code":"resync_required","generation":1}`, true},
		{"private_error", 503, `private response text`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client := capture.Client{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "token", nil }}
			_, err := client.Send(context.Background(), capture.Batch{})
			if err == nil || errors.Is(err, capture.ErrResync) != test.resync || strings.Contains(err.Error(), "private response text") {
				t.Fatal(err)
			}
		})
	}
}
