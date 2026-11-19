package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"yanshi/internal/consent"
	"yanshi/internal/httpapi"
	"yanshi/internal/memory"
)

// TestConsentHTTP：业务线服务端登记与撤回 EndUser 对健康信息的单独同意（ADR-0022 修订）。只接受服务令牌；登记之后健康信息
// 才能写入，撤回立即停止，purge 一并删除；注销账号删除同意记录，之后的登记按删除屏障返回 404。
func TestConsentHTTP(t *testing.T) {
	e := newAuthzEnv(t)
	ctx := context.Background()
	token := e.token(t, "demo/", time.Hour)
	userToken := e.token(t, "demo/u1", time.Hour)
	path := "/v1/end_users/u1/consents/health"
	request := func(method, path, tok string, want int) string {
		t.Helper()
		code, result := e.do(t, method, path, tok, nil)
		if code != want {
			t.Fatalf("%s %s = %d %s, want %d", method, path, code, result, want)
		}
		return result
	}
	var calls int
	saveHealth := func() error {
		calls++
		_, err := e.api.Memory.Save(ctx, memory.SaveRequest{BusinessLine: "demo", EndUser: "u1", CallID: "health-" + string(rune('a'+calls)),
			Category: memory.Health, Content: "对青霉素过敏"})
		return err
	}
	if err := saveHealth(); !errors.Is(err, memory.ErrRejected) {
		t.Fatalf("health saved before consent: %v", err)
	}
	request("PUT", path, userToken, http.StatusForbidden)
	request("DELETE", path, userToken, http.StatusForbidden)
	request("PUT", "/v1/end_users/u1/consents/finance", token, http.StatusBadRequest)
	request("PUT", path+"?business_line=other", token, http.StatusForbidden)
	request("PUT", path, e.token(t, "other/", time.Hour), http.StatusBadRequest) // other 未开启健康信息
	request("DELETE", path+"?purge=maybe", token, http.StatusBadRequest)
	var status struct {
		Kind      string     `json:"kind"`
		GrantedAt *time.Time `json:"granted_at"`
		Purged    *int       `json:"purged"`
	}
	body := request("PUT", path, token, http.StatusOK)
	if err := json.Unmarshal([]byte(body), &status); err != nil || status.Kind != "health" || status.GrantedAt == nil {
		t.Fatalf("consent response: %s, %v", body, err)
	}
	first := *status.GrantedAt
	// 重复登记保留最初的同意时间。
	time.Sleep(2 * time.Millisecond)
	status.GrantedAt = nil
	body = request("PUT", path, token, http.StatusOK)
	if err := json.Unmarshal([]byte(body), &status); err != nil || status.GrantedAt == nil || !status.GrantedAt.Equal(first) {
		t.Fatalf("repeated consent response: %s, %v; want granted_at %v", body, err, first)
	}
	if err := saveHealth(); err != nil {
		t.Fatalf("health rejected after consent: %v", err)
	}
	// 撤回（不清除）：立即停止写入与检索，已有的保留。
	status = struct {
		Kind      string     `json:"kind"`
		GrantedAt *time.Time `json:"granted_at"`
		Purged    *int       `json:"purged"`
	}{}
	body = request("DELETE", path, token, http.StatusOK)
	if err := json.Unmarshal([]byte(body), &status); err != nil || status.Kind != "health" || status.GrantedAt != nil || status.Purged != nil {
		t.Fatalf("withdrawal response: %s, %v", body, err)
	}
	if err := saveHealth(); !errors.Is(err, memory.ErrRejected) {
		t.Fatalf("health saved after withdrawal: %v", err)
	}
	if hits, err := e.api.Memory.Search(ctx, "u1", "demo", "", 10); err != nil || len(hits) != 1 || hits[0].Category == memory.Health {
		t.Fatalf("search after withdrawal: %+v, %v", hits, err)
	}
	if listed, err := e.api.Memory.List(ctx, "demo", "u1", memory.Health, "", false, 10); err != nil || len(listed) != 1 {
		t.Fatalf("withdrawal deleted health memories: %+v, %v", listed, err)
	}
	// 撤回并清除：健康 Memory 删除，其他类别保留。
	body = request("DELETE", path+"?purge=true", token, http.StatusOK)
	if err := json.Unmarshal([]byte(body), &status); err != nil || status.Purged == nil || *status.Purged != 1 {
		t.Fatalf("purge response: %s, %v", body, err)
	}
	if listed, err := e.api.Memory.List(ctx, "demo", "u1", "", "", true, 10); err != nil || len(listed) != 1 || listed[0].Category == memory.Health {
		t.Fatalf("after purge: %+v, %v", listed, err)
	}
	// 注销账号删除同意记录；屏障期间的登记返回 404。
	request("PUT", path, token, http.StatusOK)
	request("DELETE", "/v1/end_users/u1", token, http.StatusAccepted)
	if at, err := e.api.Service.Consents.Get(ctx, "demo", "u1", consent.Health); err != nil || !at.IsZero() {
		t.Fatalf("account deletion left consent: %v, %v", at, err)
	}
	request("PUT", path, token, http.StatusNotFound)
}

func TestConsentHTTPDevelopmentBusinessLine(t *testing.T) {
	e := newAuthzEnv(t)
	srv := httptest.NewServer((&httpapi.Server{Service: e.api.Service, Memory: e.api.Memory}).Handler())
	defer srv.Close()
	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{"PUT", "/v1/end_users/u1/consents/health", 400},
		{"PUT", "/v1/end_users/u1/consents/health?business_line=demo", 200},
		{"PUT", "/v1/end_users/u1/consents/health?business_line=other", 400},
		{"DELETE", "/v1/end_users/u1/consents/health", 400},
		{"DELETE", "/v1/end_users/u1/consents/health?business_line=demo&purge=true", 200},
	} {
		req, err := http.NewRequest(tc.method, srv.URL+tc.path, nil)
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
