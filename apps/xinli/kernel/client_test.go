package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
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
			c := &Client{BaseURL: server.URL + "/", Token: token, HTTP: server.Client()}
			sid, err := c.CreateSession(context.Background(), "bl", "u", "xinli-project")
			if err != nil || sid != "ses_1" {
				t.Fatalf("create: %q %v", sid, err)
			}
			if err := c.Submit(context.Background(), sid, "input_1", "整理 2026-10-23"); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("calls: %d", calls)
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
			submitErr := c.Submit(context.Background(), "ses", "in", "private-input")
			for _, err := range []error{createErr, submitErr} {
				if err == nil || !strings.Contains(err.Error(), fmt.Sprint(status)) || strings.Contains(err.Error(), "private-input") {
					t.Fatalf("unsafe error: %v", err)
				}
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
