package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFindInputPublicPagination(t *testing.T) {
	for _, kind := range []string{"run_requested", "steered"} {
		t.Run(kind, func(t *testing.T) {
			var cursors []string
			client := inputStatusTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/sessions/s/inputs" {
					http.NotFound(w, r)
					return
				}
				if r.URL.Path != "/v1/sessions/s/events" {
					t.Error(r.URL.Path)
				}
				after := r.URL.Query().Get("after")
				cursors = append(cursors, after)
				switch after {
				case "0":
					// 短页仍须继续；包含大正文的合法事件不能被普通响应的 1 MiB 限制截断。
					_ = json.NewEncoder(w).Encode(map[string]any{"events": []any{map[string]any{"seq": "1", "session_id": "s", "id": "e1", kind: map[string]any{"run_id": "r", "input_id": "wanted", "input": strings.Repeat("x", 2<<20)}}}})
				case "1":
					_, _ = fmt.Fprint(w, `{"events":[{"seq":"2","session_id":"s","id":"e2","run_completed":{"run_id":"r"}}]}`)
				case "2":
					_, _ = fmt.Fprint(w, `{"events":[]}`)
				default:
					t.Error("cursor", after)
					w.WriteHeader(500)
				}
			}))
			receipt, found, err := client.FindInput(context.Background(), "s", "wanted")
			if err != nil || !found || receipt.InputID != "wanted" || receipt.Sequence != 1 || receipt.EventID != "e1" || receipt.Terminal != "completed" || receipt.Steered != (kind == "steered") || len(cursors) != 3 {
				t.Fatal(receipt, found, err, cursors)
			}
		})
	}
}

func TestFindInputDoesNotInferAbsenceFromReadFailure(t *testing.T) {
	for _, body := range []string{`{}`, `{"events":null}`, `{"events":[{"seq":"0","session_id":"s"}]}`, `{"events":[{"seq":"1","session_id":"other"}]}`, `{"events":[{"seq":"no","session_id":"s"}]}`, `{"events":[`} {
		t.Run(body, func(t *testing.T) {
			client := inputStatusTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/sessions/s/inputs" {
					http.NotFound(w, r)
					return
				}
				_, _ = fmt.Fprint(w, body)
			}))
			if _, found, err := client.FindInput(context.Background(), "s", "wanted"); err == nil || found {
				t.Fatal("invalid page accepted", found, err)
			}
		})
	}
	for _, status := range []int{404, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client := inputStatusTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/sessions/s/inputs" {
					http.NotFound(w, r)
					return
				}
				if r.URL.Query().Get("after") == "0" {
					_, _ = fmt.Fprint(w, `{"events":[{"seq":"1","session_id":"s","run_requested":{"run_id":"r","input_id":"wanted"}}]}`)
					return
				}
				w.WriteHeader(status)
			}))
			if _, found, err := client.FindInput(context.Background(), "s", "wanted"); err == nil || found || (status == 404 && !errors.Is(err, ErrNotFound)) {
				t.Fatal(found, err)
			}
		})
	}
}

func TestSubmitInputReceipt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if in["input_id"] != "input" || in["text"] != "整理今天" {
			t.Error(in)
		}
		w.WriteHeader(202)
		_, _ = fmt.Fprint(w, `{"run_id":"r","steered":true,"duplicate":true}`)
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL}
	got, err := client.SubmitInput(context.Background(), "s", "input", "整理今天")
	if err != nil || got.RunID != "r" || got.InputID != "input" || !got.Steered {
		t.Fatal(got, err)
	}
}
