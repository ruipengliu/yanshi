package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"yanshi/internal/httpapi"
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

func TestTierHTTPChangesQuotaAndAccountDeletionClearsIt(t *testing.T) {
	e := newAuthzEnv(t)
	ctx := context.Background()
	token := e.token(t, "demo/", time.Hour)
	userToken := e.token(t, "demo/u1", time.Hour)
	path := "/v1/end_users/u1/tier"
	request := func(method, path, tok string, body any, want int) string {
		t.Helper()
		code, result := e.do(t, method, path, tok, body)
		if code != want {
			t.Fatalf("%s %s = %d %s, want %d", method, path, code, result, want)
		}
		return result
	}
	if err := e.api.Usage.Record(ctx, &usage.Entry{ID: "over-default", BusinessLine: "demo", EndUser: "u1", Kind: usage.Model, Cost: usage.Yuan(2), At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	check := func(exceeded bool) {
		t.Helper()
		p, err := e.api.Service.Quotas.Check(ctx, "demo", "u1", time.Now())
		if err != nil || (p != nil) != exceeded {
			t.Fatalf("Check = %+v, %v; exceeded %v", p, err, exceeded)
		}
	}
	check(true)
	request("PUT", path, userToken, map[string]string{"tier": "pro"}, http.StatusForbidden)
	request("DELETE", path, userToken, nil, http.StatusForbidden)
	request("PUT", path, token, map[string]string{"tier": "unknown"}, http.StatusBadRequest)
	request("PUT", path, token, map[string]string{"tier": "pro", "business_line": "other"}, http.StatusForbidden)
	request("DELETE", path+"?business_line=other", token, nil, http.StatusForbidden)
	request("PUT", path, e.token(t, "other/", time.Hour), map[string]string{"tier": "pro"}, http.StatusBadRequest)
	body := request("PUT", path, token, map[string]string{"tier": "pro"}, http.StatusOK)
	var status struct {
		Tier  string `json:"tier"`
		Limit int64  `json:"limit_micros"`
	}
	if err := json.Unmarshal([]byte(body), &status); err != nil || status.Tier != "pro" || status.Limit != usage.Yuan(3) {
		t.Fatalf("assignment response: %s, %v", body, err)
	}
	body = request("GET", "/v1/quota", userToken, nil, http.StatusOK)
	var quota struct{ Periods []usage.Period }
	if err := json.Unmarshal([]byte(body), &quota); err != nil || len(quota.Periods) != 2 {
		t.Fatalf("quota response: %s, %v", body, err)
	}
	if p := quota.Periods[1]; p.Tier != "pro" || p.Limit != usage.Yuan(3) || p.Spent != usage.Yuan(2)+5 || p.Exceeded {
		t.Fatalf("tier quota: %+v", p)
	}
	check(false)
	body = request("DELETE", path, token, nil, http.StatusOK)
	if err := json.Unmarshal([]byte(body), &status); err != nil || status.Tier != "" || status.Limit != usage.Yuan(1) {
		t.Fatalf("default response: %s, %v", body, err)
	}
	check(true)
	request("PUT", path, token, map[string]string{"tier": "pro"}, http.StatusOK)
	request("DELETE", "/v1/end_users/u1", token, nil, http.StatusAccepted)
	if tier, err := e.api.Service.Tiers.Get(ctx, "demo", "u1"); err != nil || tier != "" {
		t.Fatalf("account deletion left tier: %q, %v", tier, err)
	}
	request("PUT", path, token, map[string]string{"tier": "pro"}, http.StatusNotFound)
}

func TestTierHTTPDevelopmentBusinessLine(t *testing.T) {
	e := newAuthzEnv(t)
	srv := httptest.NewServer((&httpapi.Server{Service: e.api.Service}).Handler())
	defer srv.Close()
	for _, tc := range []struct {
		method string
		path   string
		body   any
		want   int
	}{
		{"PUT", "/v1/end_users/u1/tier", map[string]string{"tier": "pro"}, 400},
		{"PUT", "/v1/end_users/u1/tier", map[string]string{"business_line": "demo", "tier": "pro"}, 200},
		{"PUT", "/v1/end_users/u1/tier?business_line=demo", map[string]string{"tier": "pro"}, 200},
		{"PUT", "/v1/end_users/u1/tier?business_line=other", map[string]string{"business_line": "demo", "tier": "pro"}, 400},
		{"DELETE", "/v1/end_users/u1/tier", nil, 400},
		{"DELETE", "/v1/end_users/u1/tier?business_line=demo", nil, 200},
	} {
		req, err := http.NewRequest(tc.method, srv.URL+tc.path, jsonBody(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%s %s = %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
		}
	}
}
