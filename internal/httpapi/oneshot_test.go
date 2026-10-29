package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/artifact"
	"yanshi/internal/clock"
	"yanshi/internal/httpapi"
	"yanshi/internal/lifecycle"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/node"
	"yanshi/internal/oneshot"
	"yanshi/internal/usage"
)

type oneshotModel func(context.Context, *model.Request) (*model.Response, error)

func (f oneshotModel) Generate(ctx context.Context, req *model.Request, _ func(model.Delta)) (*model.Response, error) {
	return f(ctx, req)
}

func newOneshotCore(t *testing.T) *app.Core {
	t.Helper()
	agents, err := agentdef.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	shots, err := oneshot.NewRegistry(&oneshot.Def{Name: "brief", Version: "1", Model: "fake/model", Instructions: "概括输入。", Input: oneshot.Input{MaxChars: 100}, Concurrency: 1, MaxOutputTokens: 50,
		Output: map[string]any{"type": "object", "properties": map[string]any{"brief": map[string]any{"type": "string"}}, "required": []string{"brief"}, "additionalProperties": false}})
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.Real{}
	stores := app.Memory(clk)
	return app.NewCore(stores, app.Options{Clock: clk, Agents: agents, Oneshots: shots, OneshotMax: 2,
		Model: oneshotModel(func(context.Context, *model.Request) (*model.Response, error) {
			return &model.Response{Content: model.TextBlocks(`{"brief":"hello"}`), Usage: &v1.Usage{InputTokens: 5, OutputTokens: 3}}, nil
		}),
		Blobs: artifact.NewMemBlobs(), NodeAuth: node.InsecureDevAuth{}, Moderator: moderation.Mock{}, Meter: &usage.Meter{Store: stores.Usage}})
}

func TestOneshotHTTPIdentityAndResponse(t *testing.T) {
	e := newAuthzEnv(t)
	for _, tc := range []struct {
		principal, endUser string
		status             int
	}{
		{"demo/u1", "", 200}, {"demo/u1", "u1", 200}, {"demo/u1", "u2", 403}, {"demo/", "", 400}, {"demo/", "u2", 200},
	} {
		code, body := e.do(t, "POST", "/v1/oneshots/brief", e.token(t, tc.principal, time.Hour), map[string]string{"end_user": tc.endUser, "input": "原文"})
		if code != tc.status {
			t.Fatalf("%+v: %d %s", tc, code, body)
		}
		if code == 200 {
			var result struct {
				Name, Version, Model string
				Output               map[string]string
			}
			if err := json.Unmarshal([]byte(body), &result); err != nil || result.Name != "brief" || result.Version != "1" || result.Model != "fake/model" || result.Output["brief"] != "hello" {
				t.Fatalf("response: %s, %v", body, err)
			}
			if strings.Contains(body, "usage") {
				t.Fatal("internal usage leaked", body)
			}
		}
	}
	code, _ := e.do(t, "POST", "/v1/oneshots/brief", e.token(t, "demo/", time.Hour), map[string]string{"business_line": "other", "end_user": "u1", "input": "原文"})
	if code != 403 {
		t.Fatal("cross-business identity accepted", code)
	}
}

func TestOneshotHTTPErrorsAndInsecure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		setup       func(*app.Core)
		path, input string
		want        int
	}{
		{name: "ok", input: "你好", want: 200},
		{name: "unknown", path: "missing", input: "你好", want: 404},
		{name: "empty", want: 400},
		{name: "limit", input: strings.Repeat("中", 101), want: 400},
		{name: "input_rejected", input: "【违规测试】", want: 422},
		{name: "output_rejected", input: "你好", want: 422, setup: func(c *app.Core) {
			c.Oneshots.Model = oneshotModel(func(context.Context, *model.Request) (*model.Response, error) {
				return &model.Response{Content: model.TextBlocks("【违规测试】")}, nil
			})
		}},
		{name: "schema", input: "你好", want: 502, setup: func(c *app.Core) {
			c.Oneshots.Model = oneshotModel(func(context.Context, *model.Request) (*model.Response, error) {
				return &model.Response{Content: model.TextBlocks("private-output")}, nil
			})
		}},
		{name: "provider", input: "你好", want: 502, setup: func(c *app.Core) {
			c.Oneshots.Model = oneshotModel(func(context.Context, *model.Request) (*model.Response, error) {
				return nil, errors.New("private-input")
			})
		}},
		{name: "timeout", input: "你好", want: 504, setup: func(c *app.Core) {
			c.Oneshots.Model = oneshotModel(func(context.Context, *model.Request) (*model.Response, error) { return nil, model.ErrTimeout })
		}},
		{name: "busy", input: "你好", want: 429, setup: func(c *app.Core) {
			c.Oneshots.Model = oneshotModel(func(context.Context, *model.Request) (*model.Response, error) { return nil, model.ErrBusy })
		}},
		{name: "quota", input: "你好", want: 429, setup: func(c *app.Core) {
			if err := c.Stores.Usage.Record(context.Background(), &usage.Entry{ID: "before", BusinessLine: "demo", EndUser: "u1", Kind: usage.Model, Cost: 2_000_000, At: time.Now()}); err != nil {
				t.Fatal(err)
			}
			c.Oneshots.Quotas = &usage.Quotas{Store: c.Stores.Usage, Limits: map[string]usage.Limits{"demo": {EndUserDaily: 1}}}
		}},
		{name: "barrier", input: "你好", want: 404, setup: func(c *app.Core) {
			if _, err := c.Stores.Deletions.MarkEndUser(context.Background(), &lifecycle.EndUserTombstone{BusinessLine: "demo", EndUser: "u1", RequestID: "delete", RequestedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core := newOneshotCore(t)
			if tc.setup != nil {
				tc.setup(core)
			}
			api := &httpapi.Server{Service: core.Service, Oneshots: core.Oneshots}
			path := tc.path
			if path == "" {
				path = "brief"
			}
			b, _ := json.Marshal(map[string]string{"business_line": "demo", "end_user": "u1", "input": tc.input})
			req := httptest.NewRequest("POST", "/v1/oneshots/"+path, strings.NewReader(string(b)))
			rec := httptest.NewRecorder()
			api.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want || strings.Contains(rec.Body.String(), "private-") {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if tc.name == "quota" && (rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), `"quota"`)) {
				t.Fatal("missing quota shape")
			}
			if tc.name == "ok" {
				query := usage.Query{BusinessLine: "demo", EndUser: "u1", From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour), GroupBy: usage.ByOneshot}
				rows, err := core.Stores.Usage.Report(context.Background(), query)
				if err != nil || len(rows) != 1 || rows[0].Key != "brief@1" {
					t.Fatalf("usage %+v %v", rows, err)
				}
			}
		})
	}
	core := newOneshotCore(t)
	srv := httptest.NewServer((&httpapi.Server{Service: core.Service, Oneshots: core.Oneshots}).Handler())
	defer srv.Close()
	res, err := http.Post(srv.URL+"/v1/oneshots/brief", "application/json", strings.NewReader(`{"business_line":"demo","end_user":"u1","input":"你好"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal("real HTTP request", res.StatusCode)
	}
}

func TestOneshotHTTPPlainOutputAndUsageGrouping(t *testing.T) {
	core := newOneshotCore(t)
	r, err := oneshot.NewRegistry(&oneshot.Def{Name: "brief", Version: "1", Model: "fake/model", Instructions: "概括", Input: oneshot.Input{MaxChars: 100}, Concurrency: 1, MaxOutputTokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	core.Oneshots.Registry = r
	handler := (&httpapi.Server{Service: core.Service, Oneshots: core.Oneshots, Usage: core.Stores.Usage}).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/oneshots/brief", strings.NewReader(`{"business_line":"demo","end_user":"u1","input":"你好"}`)))
	var result struct{ Output any }
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Output.(string); !ok || rec.Code != 200 {
		t.Fatalf("plain output encoded as JSON: %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/usage?business_line=demo&end_user=u1&group_by=oneshot", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "brief@1") {
		t.Fatalf("usage group: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/oneshots/brief", strings.NewReader(`{"end_user":"u1","input":"你好"}`)))
	if rec.Code != 400 {
		t.Fatal("insecure request needs business_line", rec.Code)
	}
	rec = httptest.NewRecorder()
	(&httpapi.Server{}).Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/oneshots/brief", strings.NewReader(`{"business_line":"demo","end_user":"u1","input":"你好"}`)))
	if rec.Code != 404 {
		t.Fatal("unconfigured service", rec.Code)
	}
}
