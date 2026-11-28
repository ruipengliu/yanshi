package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHTTPContract(t *testing.T) {
	for _, token := range []string{"", "service-token"} {
		t.Run(fmt.Sprintf("token=%t", token != ""), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "POST" {
					t.Errorf("method: %s", r.Method)
				}
				wantAuth := ""
				if token != "" {
					wantAuth = "Bearer " + token
				}
				if r.Header.Get("Authorization") != wantAuth {
					t.Error("authorization mismatch")
				}
				if r.Header.Get("Content-Type") != "application/json" {
					t.Error("content type")
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				// 字段名固定对照 internal/httpapi/httpapi.go 的 create 与 submit；不导入内核以免依赖倒置。
				switch r.URL.Path {
				case "/v1/oneshots/xinli-live-brief":
					want := map[string]string{"end_user": "u", "input": "旁听原文"}
					if calls == 4 {
						want["business_line"] = "bl"
					}
					if !reflect.DeepEqual(body, want) {
						t.Errorf("oneshot fields: %v", body)
					}
					_, _ = fmt.Fprint(w, `{"version":"1","model":"bench/any","output":{"brief":"简述"}}`)
				case "/v1/sessions":
					if !reflect.DeepEqual(body, map[string]string{"business_line": "bl", "end_user": "u", "agent": "xinli-project"}) {
						t.Errorf("create fields: %v", body)
					}
					w.WriteHeader(http.StatusCreated)
					_, _ = fmt.Fprint(w, `{"session_id":"ses_1"}`)
				case "/v1/sessions/ses_1/inputs":
					if !reflect.DeepEqual(body, map[string]string{"input_id": "input_1", "text": "整理 2026-10-23"}) {
						t.Errorf("submit fields: %v", body)
					}
					w.WriteHeader(http.StatusAccepted)
				default:
					t.Errorf("path: %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			c := &Client{BaseURL: server.URL + "/", Tokens: testTokens(token), HTTP: server.Client()}
			sid, err := c.CreateSession(context.Background(), "bl", "u", "xinli-project")
			if err != nil || sid != "ses_1" {
				t.Fatalf("create: %q %v", sid, err)
			}
			if err := c.Submit(WithBusinessLine(context.Background(), "bl"), sid, "input_1", "整理 2026-10-23"); err != nil {
				t.Fatal(err)
			}
			shot, err := c.Oneshot(WithBusinessLine(context.Background(), "bl"), "u", "xinli-live-brief", "旁听原文")
			if err != nil || shot.Version != "1" || shot.Model != "bench/any" || string(shot.Output) != `{"brief":"简述"}` {
				t.Fatalf("oneshot: %+v %v", shot, err)
			}
			if _, err := c.OneshotForBusinessLine(context.Background(), "bl", "u", "xinli-live-brief", "旁听原文"); err != nil {
				t.Fatal(err)
			}
			if calls != 4 {
				t.Fatalf("calls: %d", calls)
			}
		})
	}
}

func TestIdempotentSessionCreation(t *testing.T) {
	deadline := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	opts := SessionOptions{IdempotencyKey: "xc-example", DeleteAt: deadline}
	for _, status := range []int{http.StatusCreated, http.StatusOK, http.StatusNotFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/sessions" {
					t.Errorf("request: %s %s", r.Method, r.URL.Path)
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				want := map[string]string{"business_line": "bl", "end_user": "u", "agent": "xinli-day",
					"delete_at": deadline.Format(time.RFC3339), "idempotency_key": opts.IdempotencyKey}
				if !reflect.DeepEqual(body, want) {
					t.Errorf("create fields: %v", body)
				}
				w.WriteHeader(status)
				if status == http.StatusNotFound {
					_, _ = fmt.Fprint(w, `{"error":"not found"}`)
					return
				}
				_, _ = fmt.Fprint(w, `{"session_id":"ses_1"}`)
			}))
			defer server.Close()
			c := &Client{BaseURL: server.URL}
			sid, err := c.CreateSessionWithOptions(context.Background(), "bl", "u", "xinli-day", opts)
			if status == http.StatusNotFound {
				if sid != "" || !errors.Is(err, ErrNotFound) {
					t.Fatalf("deleted key: %q %v", sid, err)
				}
			} else if sid != "ses_1" || err != nil {
				t.Fatalf("create: %q %v", sid, err)
			}
		})
	}
}

func TestErrorsDoNotExposeContent(t *testing.T) {
	for _, status := range []int{302, 400, 401, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, "private-input")
			}))
			defer server.Close()
			c := &Client{BaseURL: server.URL}
			_, createErr := c.CreateSession(context.Background(), "bl", "private-input", "agent")
			submitErr := c.Submit(WithBusinessLine(context.Background(), "bl"), "ses", "in", "private-input")
			_, shotErr := c.Oneshot(WithBusinessLine(context.Background(), "bl"), "u", "xinli-live-brief", "private-input")
			for _, err := range []error{createErr, submitErr, shotErr} {
				if err == nil || !strings.Contains(err.Error(), fmt.Sprint(status)) || strings.Contains(err.Error(), "private-input") {
					t.Fatalf("unsafe error: %v", err)
				}
			}
		})
	}
}

type errorResponseTransport struct {
	status int
	body   string
}

func (r errorResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: r.status, Body: io.NopCloser(strings.NewReader(r.body))}, nil
}

func TestHTTPErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name, body  string
		status      int
		code        string
		reason      string
		outboxReset bool
	}{
		{"结构化重答优先", `{"error":"conflict: redo: not_last_run","code":"conflict","reason":"session_closed"}`, 409, "conflict", "session_closed", false},
		{"旧内核重答", `{"error":"conflict: redo: not_last_run"}`, 409, "", "not_last_run", false},
		{"未知原因", `{"error":"conflict: redo: not_last_run","code":"conflict","reason":"private-input"}`, 409, "conflict", "", false},
		{"未知分类", `{"error":"private-input","code":"private_input","reason":"not_last_run"}`, 409, "", "", false},
		{"分类不符", `{"error":"private-input","code":"memory_limit","reason":"not_last_run"}`, 409, "memory_limit", "", false},
		{"结构化过滤变化", `{"error":"private-input","code":"conflict","reason":"outbox_filter_changed"}`, 409, "conflict", "outbox_filter_changed", false},
		{"旧内核过滤变化", `{"error":"outbox_filter_changed"}`, 409, "", "outbox_filter_changed", false},
		{"结构化游标过期", `{"error":"private-input","code":"gone","reason":"outbox_cursor_expired"}`, 410, "gone", "outbox_cursor_expired", true},
		{"旧内核游标过期", `{"error":"outbox_cursor_expired"}`, 410, "", "outbox_cursor_expired", true},
		{"结构化世代变化", `{"error":"private-input","code":"gone","reason":"outbox_epoch_changed"}`, 410, "gone", "outbox_epoch_changed", true},
		{"旧内核世代变化", `{"error":"outbox_epoch_changed"}`, 410, "", "outbox_epoch_changed", true},
		{"其他状态", `{"error":"private-input","code":"not_found"}`, 404, "not_found", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{BaseURL: "http://kernel", HTTP: &http.Client{Transport: errorResponseTransport{tc.status, tc.body}}}
			_, err := c.ThreadRequest(t.Context(), "bl", "sid", http.MethodPost, "/inputs", nil)
			var remote *HTTPError
			if !errors.As(err, &remote) || remote.StatusCode != tc.status || remote.Code != tc.code || remote.Reason != tc.reason {
				t.Fatalf("error = %#v, want status=%d code=%q reason=%q", err, tc.status, tc.code, tc.reason)
			}
			if OutboxResetRequired(err) != tc.outboxReset || err.Error() != fmt.Sprintf("kernel: HTTP status %d", tc.status) {
				t.Fatalf("error = %v, outbox reset = %v", err, OutboxResetRequired(err))
			}
		})
	}
}

func TestInvalidCreateResponse(t *testing.T) {
	for _, body := range []string{`{}`, `not JSON`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, body) }))
		_, err := (&Client{BaseURL: server.URL}).CreateSession(context.Background(), "bl", "u", "agent")
		server.Close()
		if err == nil {
			t.Fatal("accepted invalid response")
		}
	}
}

type fixedToken string

func (t fixedToken) Token(context.Context, string) (string, error) { return string(t), nil }
func testTokens(value string) TokenSource {
	if value == "" {
		return nil
	}
	return fixedToken(value)
}
