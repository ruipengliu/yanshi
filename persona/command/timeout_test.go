package command_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"persona/command"
	"persona/query"
	"persona/store"
)

// 自定义 Client 没有 Timeout 也必须受请求预算限制；半截成功响应不能透传给 BFF。
func TestPersonaClientsBoundIncompleteResponse(t *testing.T) {
	for _, method := range []string{"query", "command"} {
		t.Run(method, func(t *testing.T) {
			release := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"partial":`))
				w.(http.Flusher).Flush()
				<-release
			}))
			defer upstream.Close()
			defer close(release)
			token := func(context.Context, string) (string, error) { return "service", nil }
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			started := time.Now()
			var err error
			var result []byte
			owner := store.Owner{BusinessLine: "bl", EndUser: "u"}
			if method == "query" {
				client := &query.Client{BaseURL: upstream.URL, Token: token, HTTP: upstream.Client()}
				result, err = client.Read(ctx, owner, query.Request{Resource: "day", Date: "2026-10-29", Limit: 30})
			} else {
				client := &command.Client{BaseURL: upstream.URL, Token: token, HTTP: upstream.Client()}
				result, err = client.Execute(ctx, owner, command.Request{Operation: "erase"})
			}
			var unavailable *query.Error
			if !errors.As(err, &unavailable) || unavailable.Status != http.StatusServiceUnavailable || len(result) != 0 || time.Since(started) >= 2500*time.Millisecond {
				t.Fatalf("method=%s err=%v bytes=%d elapsed=%s", method, err, len(result), time.Since(started))
			}
			resp := httptest.NewRecorder()
			if method == "query" {
				query.WriteError(resp, err)
			} else {
				command.WriteError(resp, err)
			}
			if resp.Code != http.StatusServiceUnavailable || resp.Header().Get("Retry-After") != "1" {
				t.Fatal("upstream failure lost retryable HTTP response")
			}
		})
	}
}

// 等响应头是服务端正常执行时间，不能当作一秒建连失败；查询和命令均可超过两秒。
func TestPersonaClientsAllowSlowSuccess(t *testing.T) {
	for _, method := range []string{"query", "command"} {
		t.Run(method, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-time.After(2300 * time.Millisecond):
					_, _ = w.Write([]byte(`{}`))
				case <-r.Context().Done():
				}
			}))
			defer upstream.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			token := func(context.Context, string) (string, error) { return "service", nil }
			owner := store.Owner{BusinessLine: "bl", EndUser: "u"}
			var err error
			if method == "query" {
				_, err = (&query.Client{BaseURL: upstream.URL, Token: token}).Read(ctx, owner, query.Request{Resource: "day", Date: "2026-10-29", Limit: 30})
			} else {
				_, err = (&command.Client{BaseURL: upstream.URL, Token: token}).Execute(ctx, owner, command.Request{Operation: "erase"})
			}
			if err != nil {
				t.Fatalf("slow healthy %s rejected: %v", method, err)
			}
		})
	}
}
