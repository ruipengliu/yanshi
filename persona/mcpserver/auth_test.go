package mcpserver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"persona/store/memstore"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerTransport struct {
	token *atomic.Value
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token.Load().(string))
	return http.DefaultTransport.RoundTrip(r)
}

func TestCredentialMatrix(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	var unix atomic.Int64
	unix.Store(now.Unix())
	credentials := []Credential{
		{BusinessLine: "bl", Token: "old", ExpiresAt: now.Add(time.Hour)},
		{BusinessLine: "bl", Token: "new", ExpiresAt: now.Add(2 * time.Hour)},
		{BusinessLine: "other", Token: "other-key", ExpiresAt: now.Add(2 * time.Hour)},
		{BusinessLine: "bl", Token: "expired", ExpiresAt: now},
	}
	hs := httptest.NewServer(Handler(memstore.New(), Options{Credentials: credentials, AllowUnmanaged: true, Now: func() time.Time {
		return time.Unix(unix.Load(), 0)
	}}))
	defer hs.Close()
	for _, token := range []string{"", "wrong", "expired"} {
		req, _ := http.NewRequest("POST", hs.URL, strings.NewReader(`{"private":"content"}`))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 401 || string(body) != "unauthorized\n" {
			t.Fatalf("unsafe rejection: %d %s", resp.StatusCode, body)
		}
	}
	for _, token := range []string{"old", "new"} {
		t.Run(token, func(t *testing.T) {
			var current atomic.Value
			current.Store(token)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
			conn, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: hs.URL, HTTPClient: &http.Client{Transport: bearerTransport{&current}}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			for _, line := range []string{"bl", "other", ""} {
				for _, tool := range []string{"find_related", "captures"} {
					args := map[string]any{"query": "none"}
					if tool == "captures" {
						args = map[string]any{"after": "0"}
					}
					result, err := conn.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args, Meta: mcp.Meta{"yanshi/business_line": line, "yanshi/end_user": "u", "yanshi/session": "s"}})
					if line != "bl" {
						if err == nil && (result == nil || !result.IsError) {
							t.Fatal("business line mismatch accepted")
						}
						if err != nil && !strings.Contains(err.Error(), "unauthorized") {
							t.Fatalf("unexpected error: %v", err)
						}
					} else if err != nil || tool == "find_related" && result.IsError {
						t.Fatalf("valid credential failed: %v %+v", err, result)
					}
				}
			}
			if token == "old" {
				current.Store("new")
				if err := conn.Ping(ctx, nil); err != nil {
					t.Fatal("rotation could not reuse the same business line session")
				}
				current.Store("old")
			}
			// 用未受切换请求影响的连接验证到期；不能把此前的断连误当作期限校验成功。
			unix.Store(now.Add(3 * time.Hour).Unix())
			if err := conn.Ping(ctx, nil); err == nil {
				t.Fatal("existing connection bypassed expiration")
			}
			unix.Store(now.Unix())

		})
	}
}

func TestHandlerFailsClosed(t *testing.T) {
	for _, opts := range []Options{{}, {Credentials: []Credential{{BusinessLine: "bl", Token: "secret"}}}, {Credentials: []Credential{{BusinessLine: "bl", Token: "same", ExpiresAt: time.Now().Add(time.Hour)}, {BusinessLine: "other", Token: "same", ExpiresAt: time.Now().Add(time.Hour)}}}} {
		rr := httptest.NewRecorder()
		Handler(memstore.New(), opts).ServeHTTP(rr, httptest.NewRequest("POST", "/mcp", nil))
		if rr.Code != 401 {
			t.Fatal("missing or invalid configuration accepted")
		}
	}
}

// 将凭据换成另一业务线时，即便沿用客户端 MCP Session，也不能继承原来的身份。
func TestCredentialSwitchCannotReuseSession(t *testing.T) {
	now := time.Now()
	hs := httptest.NewServer(Handler(memstore.New(), Options{AllowUnmanaged: true, Credentials: []Credential{
		{BusinessLine: "bl", Token: "first", ExpiresAt: now.Add(time.Hour)},
		{BusinessLine: "other", Token: "second", ExpiresAt: now.Add(time.Hour)},
	}}))
	defer hs.Close()
	var current atomic.Value
	current.Store("first")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: hs.URL, HTTPClient: &http.Client{Transport: bearerTransport{&current}}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Ping(ctx, nil); err != nil {
		t.Fatal(err)
	}
	current.Store("second")
	result, err := conn.CallTool(ctx, &mcp.CallToolParams{Name: "find_related", Arguments: map[string]string{"query": "none"}, Meta: mcp.Meta{
		"yanshi/business_line": "bl", "yanshi/end_user": "u", "yanshi/session": "s",
	}})
	if err == nil && !result.IsError {
		t.Fatal("credential switch reused identity")
	}
}

func credentialHTTPClient(token string) *http.Client {
	var current atomic.Value
	current.Store(token)
	return &http.Client{Transport: bearerTransport{&current}}
}

// 拨业务日不能绕过或提前触发凭据到期；认证仍按独立时钟检查每次请求。
func TestAuthClockIndependentFromBusinessClock(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	var authUnix atomic.Int64
	authUnix.Store(now.Unix())
	hs := httptest.NewServer(Handler(memstore.New(), Options{
		AllowUnmanaged: true,
		Credentials:    []Credential{{BusinessLine: "bl", Token: "key", ExpiresAt: now.Add(time.Hour)}},
		Now:            func() time.Time { return now.AddDate(10, 0, 0) },
		AuthNow:        func() time.Time { return time.Unix(authUnix.Load(), 0) },
	}))
	defer hs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: hs.URL, HTTPClient: credentialHTTPClient("key"), DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Ping(ctx, nil); err != nil {
		t.Fatal("business clock expired valid credential", err)
	}
	authUnix.Store(now.Add(2 * time.Hour).Unix())
	if err := conn.Ping(ctx, nil); err == nil {
		t.Fatal("auth clock expiration was ignored")
	}
}
