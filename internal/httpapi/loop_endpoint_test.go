package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"yanshi/internal/auth"
	"yanshi/internal/extloop"
)

type rejectAll struct{}

func (rejectAll) Verify(string) (auth.Principal, error) {
	return auth.Principal{}, errors.New("rejected")
}

// 外部循环的模型端点只认步令牌（ADR-0063 第 2 条）：它不经业务线令牌的中间件，其余 /v1/ 接口照旧要业务线令牌。
func TestLoopEndpointBypassesTokenMiddleware(t *testing.T) {
	reached := false
	h := (&Server{Auth: rejectAll{}, LoopModels: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusTeapot)
	})}).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, extloop.EndpointPath, nil))
	if !reached || rec.Code != http.StatusTeapot {
		t.Fatalf("endpoint not reached: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("other routes: %d, want 401", rec.Code)
	}
	// 没有装配端点时，这个路径照常经中间件。
	rec = httptest.NewRecorder()
	(&Server{Auth: rejectAll{}}).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, extloop.EndpointPath, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("without endpoint: %d, want 401", rec.Code)
	}
}
