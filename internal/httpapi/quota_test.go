package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"yanshi/internal/usage"
)

// TestQuotaRejectsNewRunsOnly：配额用尽时新 Run 返回 429 与 Retry-After，进行中 Run 的插话仍被接受。
func TestQuotaRejectsNewRunsOnly(t *testing.T) {
	e := newAuthzEnv(t)
	tok := e.token(t, "demo/u1", time.Hour)
	code, body := e.do(t, "POST", "/v1/sessions", tok, map[string]string{"agent": "echo"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal([]byte(body), &created)

	_ = e.api.Usage.Record(context.Background(), &usage.Entry{ID: "big", BusinessLine: "demo", EndUser: "u1", Kind: usage.Model, Cost: usage.Yuan(1), At: time.Now()})

	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/sessions/"+created.SessionID+"/inputs", jsonBody(map[string]string{"text": "hi"}))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("new run over quota: %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	// 已有进行中 Run 的 Session：插话不受配额限制。
	if code, body := e.do(t, "POST", "/v1/sessions/"+e.sid+"/inputs", tok, map[string]string{"text": "more"}); code != http.StatusAccepted {
		t.Fatalf("steer over quota: %d %s", code, body)
	}
	code, body = e.do(t, "GET", "/v1/quota", tok, nil)
	var q struct{ Periods []usage.Period }
	if err := json.Unmarshal([]byte(body), &q); code != 200 || err != nil || len(q.Periods) != 2 {
		t.Fatalf("quota: %d %s", code, body)
	}
	for _, p := range q.Periods {
		if p.Scope == "end_user" && (!p.Exceeded || p.Spent != usage.Yuan(1)+5) {
			t.Fatalf("end user period %+v", p)
		}
	}
}

func jsonBody(v any) *bytes.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}
