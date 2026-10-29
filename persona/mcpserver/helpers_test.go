package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"time"

	"persona/domain"
	"persona/fixture"
	"persona/store"
	"persona/store/memstore"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 移植的用例保留原操作形状，夹具改走 HTTP，工具调用走同一个事务适配器。
type (
	Project        = domain.Project
	Day            = domain.Day
	Awareness      = domain.Awareness
	State          = domain.State
	Commitment     = domain.Commitment
	Revision       = domain.Revision
	SeedData       = domain.SeedData
	SeedCommitment = domain.SeedCommitment
	SeedRevision   = domain.SeedRevision
	SeedExchange   = domain.SeedExchange
	Citation       = domain.Citation
)

const (
	NewProject    = domain.NewProject
	AuthorDay     = domain.AuthorDay
	AuthorUser    = domain.AuthorUser
	AuthorProject = domain.AuthorProject
)

type testServer struct {
	store    store.Store
	Now      func() time.Time
	notifier Notifier
}

func newServer(notifier Notifier) *testServer {
	return &testServer{store: memstore.New(captureClock), notifier: notifier, Now: func() time.Time { return time.Date(2026, 10, 23, 22, 0, 0, 0, shanghai) }}
}
func (x *testServer) now() time.Time { return x.Now() }
func (x *testServer) request(method, bl, eu, op string, input, output any) error {
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req := httptest.NewRequest(method, "/v1/fixture/"+bl+"/"+eu+"/"+op, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	fixture.Handler(x.store, fixture.Options{Now: x.now}).ServeHTTP(rec, req)
	if rec.Code != 200 {
		var value struct{ Error string }
		if err := json.Unmarshal(rec.Body.Bytes(), &value); err != nil {
			return err
		}
		return fmt.Errorf("%s", value.Error)
	}
	if output != nil {
		return json.Unmarshal(rec.Body.Bytes(), output)
	}
	return nil
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}
func (x *testServer) Seed(bl, eu string, d SeedData) { must(x.request("POST", bl, eu, "seed", d, nil)) }
func (x *testServer) State(bl, eu string) State {
	var st State
	must(x.request("GET", bl, eu, "state", nil, &st))
	return st
}
func (x *testServer) Bind(bl, eu, p, sid string) {
	must(x.request("POST", bl, eu, "bind", M{"project": p, "session": sid}, nil))
}
func (x *testServer) Submitted(bl, eu, sid, input string) {
	must(x.request("POST", bl, eu, "submitted", M{"session": sid, "input": input}, nil))
}
func (x *testServer) Edit(bl, eu, id, status, due string) error {
	return x.request("POST", bl, eu, "edit", M{"id": id, "status": status, "due_date": due}, nil)
}
func (x *testServer) Resolve(bl, eu, sid, ex, p string) error {
	return x.request("POST", bl, eu, "resolve", M{"session": sid, "exchange": ex, "project": p}, nil)
}
func (x *testServer) ResolveMerge(bl, eu, id string, accept bool) error {
	return x.request("POST", bl, eu, "resolve_merge", M{"id": id, "accept": accept}, nil)
}
func (x *testServer) Archive(bl, eu string) []string {
	var ids []string
	must(x.request("POST", bl, eu, "archive", M{}, &ids))
	return ids
}
func (x *testServer) ReviewWeek(bl, eu string) domain.WeekPlan {
	var plan domain.WeekPlan
	must(x.request("GET", bl, eu, "week", nil, &plan))
	return plan
}
func (x *testServer) BindWeek(bl, eu, from, sid string) {
	must(x.request("POST", bl, eu, "bind_week", M{"from": from, "session": sid}, nil))
}

func invoke(x *testServer, session, name string, args any, cites ...Citation) string {
	raw, err := json.Marshal(args)
	must(err)
	for _, tool := range tools {
		if tool.tool.Name == name {
			res, err := newRuntime(x.store, Options{Now: x.now, Notifier: x.notifier}).call(context.Background(),
				mcp.Meta{
					"yanshi/business_line": "bl", "yanshi/end_user": "u", "yanshi/session": session, "yanshi/captures": cites,
				}, raw, tool.handler, tool.tool.Annotations != nil && tool.tool.Annotations.ReadOnlyHint)
			must(err)
			return res.Content[0].(*mcp.TextContent).Text
		}
	}
	panic(name)
}

type filedRecord struct {
	bl, eu, session, date string
	projects              []string
}
type recordFiled struct{ days []filedRecord }

func (r *recordFiled) DayFiled(_ context.Context, bl, eu, session, date string, projects []string) {
	r.days = append(r.days, filedRecord{bl, eu, session, date, projects})
}
