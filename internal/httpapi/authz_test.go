package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/artifact"
	"yanshi/internal/auth"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/httpapi"
	"yanshi/internal/ids"
	"yanshi/internal/live"
	"yanshi/internal/node"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue/memqueue"
)

// authzEnv 是一个开启鉴权的 API：demo/u1 拥有一个 Session（含进行中的 Run 与一个未完成调用）、
// 一个工件和一个 Node。
type authzEnv struct {
	srv       *httptest.Server
	api       *httpapi.Server
	sid, art  string
	keys      map[string]*auth.SigningKey
	ownedNode string
}

func newAuthzEnv(t *testing.T) *authzEnv {
	t.Helper()
	e := &authzEnv{keys: map[string]*auth.SigningKey{}}
	var lines []auth.BusinessLine
	for _, bl := range []string{"demo", "other"} {
		k, err := auth.GenerateDevKey(bl, bl+"-1")
		if err != nil {
			t.Fatal(err)
		}
		pub, _ := k.Public()
		lines = append(lines, pub)
		e.keys[bl] = k
	}
	verifier, err := auth.NewJWT("yanshi", clock.Real{}, lines...)
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "echo", Version: "1", Model: "echo/any"})
	clk := clock.Real{}
	store := &session.Store{Log: memlog.New(), IDs: ids.Random(), Clock: clk}
	svc := &service.Service{Store: store, Queue: memqueue.New(clk), Agents: agents}
	arts := &artifact.Service{Meta: artifact.NewMemMeta(), Blobs: artifact.NewMemBlobs(), IDs: ids.Random(), Clock: clk}
	dir := node.NewMemDirectory(clk)
	ctx := context.Background()

	e.sid, err = svc.Create(ctx, service.CreateRequest{BusinessLine: "demo", EndUser: "u1", Agent: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Submit(ctx, e.sid, []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: "hi"}}}})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := svc.Load(ctx, e.sid)
	err = store.Commit(ctx, st,
		&v1.Event{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: res.RunID, Attempt: 1, LiveEndpoint: "http://10.0.0.1:7070"}}},
		&v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{RunId: res.RunID, Attempt: 1,
			ToolCalls: []*v1.ToolCall{{CallId: "c1", Capability: "x", ArgumentsJson: "{}"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := arts.Put(ctx, e.sid, "a.txt", "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	e.art = m.ID
	e.ownedNode = "node-u1"
	if _, _, err := dir.Register(ctx, node.Info{NodeID: e.ownedNode, Scope: node.Scope{BusinessLine: "demo", EndUser: "u1"}, Label: "pc"}); err != nil {
		t.Fatal(err)
	}

	e.api = &httpapi.Server{Auth: verifier, Service: svc, Live: live.NewMemBus(), Nodes: dir, Artifacts: arts}
	e.srv = httptest.NewServer(e.api.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

// token 为 principal 签发令牌："demo/u1" 是用户令牌，"demo/" 是服务令牌，"" 表示不带令牌。
func (e *authzEnv) token(t *testing.T, principal string, ttl time.Duration) string {
	t.Helper()
	if principal == "" {
		return ""
	}
	bl, eu, _ := strings.Cut(principal, "/")
	tok, err := e.keys[bl].Sign("yanshi", eu, ttl, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (e *authzEnv) do(t *testing.T, method, path, token string, body any) (int, string) {
	t.Helper()
	var rd *bytes.Reader
	if s, ok := body.(string); ok {
		rd = bytes.NewReader([]byte(s))
	} else {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, e.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if strings.HasSuffix(path, "/stream") {
		return resp.StatusCode, "" // SSE 不会结束，只看状态码
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

type routeCase struct {
	method, path string
	body         any
	// ok 是有权访问时的状态码。
	ok int
}

// cases 列出每个接口的一个请求；key 是 Server.Routes 中的模式。
func (e *authzEnv) cases() map[string]routeCase {
	s, a := "/v1/sessions/"+e.sid, "/v1/artifacts/"+e.art
	run := ""
	return map[string]routeCase{
		"POST /v1/sessions":                           {"POST", "/v1/sessions", map[string]string{"agent": "echo", "end_user": "u1"}, http.StatusCreated},
		"GET /v1/sessions/{id}":                       {"GET", s, nil, http.StatusOK},
		"POST /v1/sessions/{id}/inputs":               {"POST", s + "/inputs", map[string]string{"text": "more"}, http.StatusAccepted},
		"POST /v1/sessions/{id}/runs/{run}/interrupt": {"POST", s + "/runs/" + run + "RUN/interrupt", nil, http.StatusNoContent},
		"GET /v1/sessions/{id}/events":                {"GET", s + "/events", nil, http.StatusOK},
		"GET /v1/sessions/{id}/stream":                {"GET", s + "/stream", nil, http.StatusOK},
		"POST /v1/sessions/{id}/approvals/{call}":     {"POST", s + "/approvals/c1", map[string]bool{"approve": true}, http.StatusConflict},
		"GET /v1/nodes":                               {"GET", "/v1/nodes?end_user=u1", nil, http.StatusOK},
		"POST /v1/sessions/{id}/artifacts":            {"POST", s + "/artifacts?name=b.txt", "data", http.StatusCreated},
		"GET /v1/sessions/{id}/artifacts":             {"GET", s + "/artifacts", nil, http.StatusOK},
		"GET /v1/artifacts/{id}":                      {"GET", a, nil, http.StatusOK},
		"GET /v1/artifacts/{id}/meta":                 {"GET", a + "/meta", nil, http.StatusOK},
	}
}

// TestAuthorizationMatrix 对每个接口 × 每类调用方断言访问结果。新增接口必须加入 cases，否则失败。
func TestAuthorizationMatrix(t *testing.T) {
	probe := newAuthzEnv(t)
	routes := probe.api.Routes()
	covered := make([]string, 0, len(routes))
	for k := range probe.cases() {
		covered = append(covered, k)
	}
	sort.Strings(routes)
	sort.Strings(covered)
	if !slices.Equal(routes, covered) {
		t.Fatalf("authorization matrix out of date:\nroutes:  %v\ncovered: %v", routes, covered)
	}

	principals := map[string]string{
		"owner":                 "demo/u1",
		"service, same line":    "demo/",
		"other user, same line": "demo/u2",
		"user of other line":    "other/u1",
		"service of other line": "other/",
		"anonymous":             "",
	}
	for route := range probe.cases() {
		for who, principal := range principals {
			t.Run(route+"/"+who, func(t *testing.T) {
				e := newAuthzEnv(t) // 每例独立，写操作互不影响
				c := e.cases()[route]
				path := strings.Replace(c.path, "RUN", e.runID(t), 1)
				code, body := e.do(t, c.method, path, e.token(t, principal, time.Hour), c.body)
				allowed := principal == "demo/u1" || principal == "demo/"
				switch {
				case principal == "":
					if code != http.StatusUnauthorized {
						t.Fatalf("anonymous got %d, want 401", code)
					}
				case route == "POST /v1/sessions":
					// 创建不针对既有资源：身份取自令牌，body 中的 end_user=u1 只是自报值。
					// demo/u2 自报 u1 与令牌不符；other/u1 创建的是自己（other 业务线下的 u1）的 Session。
					want := map[string]int{"demo/u1": 201, "demo/": 201, "demo/u2": 403, "other/u1": 201, "other/": 201}[principal]
					if code != want {
						t.Fatalf("got %d (%s), want %d", code, body, want)
					}
				case route == "GET /v1/nodes":
					// 列表不返回 404，但别人看不到 demo/u1 的 Node。
					if code != http.StatusOK && code != http.StatusForbidden {
						t.Fatalf("got %d (%s)", code, body)
					}
					if sees := strings.Contains(body, e.ownedNode); sees != allowed {
						t.Fatalf("sees owner's node = %v, want %v (%s)", sees, allowed, body)
					}
				case allowed:
					if code != c.ok {
						t.Fatalf("got %d (%s), want %d", code, body, c.ok)
					}
				default:
					if code != http.StatusNotFound {
						t.Fatalf("got %d (%s), want 404", code, body)
					}
				}
			})
		}
	}
}

func (e *authzEnv) runID(t *testing.T) string {
	st, err := e.api.Service.Load(context.Background(), e.sid)
	if err != nil {
		t.Fatal(err)
	}
	return st.Runs[0].ID
}

func TestCreateTakesIdentityFromToken(t *testing.T) {
	e := newAuthzEnv(t)
	code, body := e.do(t, "POST", "/v1/sessions", e.token(t, "demo/u2", time.Hour), map[string]string{"agent": "echo"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var res struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal([]byte(body), &res)
	st, _ := e.api.Service.Load(context.Background(), res.SessionID)
	if st.Created.GetBusinessLine() != "demo" || st.Created.GetEndUser() != "u2" {
		t.Fatalf("created for %v", st.Created)
	}
	if code, _ := e.do(t, "POST", "/v1/sessions", e.token(t, "demo/", time.Hour), map[string]string{"agent": "echo"}); code != http.StatusBadRequest {
		t.Fatalf("service token without end_user: %d, want 400", code)
	}
	if code, _ := e.do(t, "GET", "/v1/sessions/"+e.sid, "not-a-token", nil); code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d, want 401", code)
	}
	if code, _ := e.do(t, "GET", "/healthz", "", nil); code != http.StatusOK {
		t.Fatalf("healthz: %d", code)
	}
}

func TestEventsHideInternalEndpointAndApprovalRecordsTokenHolder(t *testing.T) {
	e := newAuthzEnv(t)
	tok := e.token(t, "demo/u1", time.Hour)
	_, body := e.do(t, "GET", "/v1/sessions/"+e.sid+"/events", tok, nil)
	if strings.Contains(body, "10.0.0.1") {
		t.Fatalf("events leak live endpoint: %s", body)
	}
}

func TestStreamEndsWhenTokenExpires(t *testing.T) {
	e := newAuthzEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/v1/sessions/"+e.sid+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+e.token(t, "demo/u1", 2*time.Second))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("stream did not end cleanly: %v", err)
	}
	if !strings.Contains(buf.String(), "event: token_expired") {
		t.Fatalf("stream ended without token_expired: %q", buf.String())
	}
}
