package fixture_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"persona/domain"
	"persona/fixture"
	"persona/mcpserver"
	"persona/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSeedToolState(t *testing.T) {
	fixtureStores(t, testSeedToolState)
}

func testSeedToolState(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := func() time.Time { return time.Date(2026, 10, 23, 22, 0, 0, 0, time.UTC) }
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.Handler(s, mcpserver.Options{Now: now}))
	mux.Handle("/v1/fixture/", fixture.Handler(s, fixture.Options{Now: now}))
	hs := httptest.NewServer(mux)
	defer hs.Close()
	seed := domain.SeedData{Projects: []domain.Project{{ID: "P-1", Name: "办公室"}}, Commitments: []domain.SeedCommitment{{ID: "C-1", Kind: "mine", Content: "发报价", Project: "P-1"}}}
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	response, err := hs.Client().Post(hs.URL+"/v1/fixture/bl/u/seed", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.Status)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "fixture-test", Version: "1"}, nil)
	conn, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: hs.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	result, err := conn.CallTool(ctx, &mcp.CallToolParams{Name: "file_day", Arguments: map[string]any{"day": map[string]string{"headline": "装修定稿"}}, Meta: mcp.Meta{
		"yanshi/business_line": "bl", "yanshi/end_user": "u", "yanshi/session": "day",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "已收到") {
		t.Fatalf("tool = %+v", result)
	}
	response, err = hs.Client().Get(hs.URL + "/v1/fixture/bl/u/state")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var state domain.State
	if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if len(state.Days) != 1 || state.Days[0].Headline != "装修定稿" || state.Days[0].Session != "day" || len(state.Commitments) != 1 || state.Commitments[0].Content != "发报价" {
		t.Fatalf("state = %+v", state)
	}
}

func TestErrors(t *testing.T) {
	fixtureStores(t, testErrors)
}

func testErrors(t *testing.T, s store.Store) {
	handler := fixture.Handler(s, fixture.Options{})
	request := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, "/v1/fixture/bl/u/"+path, strings.NewReader(body)))
		return rec
	}
	for _, op := range []string{"seed", "bind", "submitted", "edit", "resolve", "resolve_merge", "archive", "bind_week", "captures", "bind_day", "capture_position", "capture_preview", "reset", "erase", "erase_day"} {
		for _, body := range []string{"{", `{} {}`, `{"unexpected":true}`} {
			t.Run(op+"/"+body, func(t *testing.T) {
				rec := request("POST", op, body)
				if rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid JSON") {
					t.Fatalf("%d %s", rec.Code, rec.Body.String())
				}
			})
		}
	}
	rec := request("POST", "seed", `{"commitments":[{"id":"C-1","kind":"mine","content":"发报价"}]}`)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	for _, tc := range []struct{ op, body, want string }{
		{"edit", `{"id":"missing"}`, "no commitment"},
		{"edit", `{"id":"C-1","status":"invalid"}`, "status must be"},
		{"edit", `{"id":"C-1","due_date":"2026-02-30"}`, "due_date must be"},
		{"edit", `{"id":"C-1"}`, "nothing to change"},
		{"resolve", `{"session":"day","exchange":"missing"}`, "is not pending"},
		{"resolve_merge", `{"id":"missing","accept":true}`, "is not pending"},
		{"bind_week", `{"from":"bad","session":"week"}`, "from must be"},
		{"bind_week", `{"from":"2026-10-17"}`, "session is required"},
	} {
		t.Run(tc.op+tc.body, func(t *testing.T) {
			rec := request("POST", tc.op, tc.body)
			if rec.Code != 400 || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
		})
	}
	// Bind 的不存在项目与 Submitted 的任意文字沿用领域行为；非法传输已在上面逐个拒绝。
	for _, tc := range []struct{ op, body string }{{"bind", `{"project":"missing","session":"s"}`}, {"submitted", `{"session":"s","input":"整理今天"}`}, {"archive", `{}`}} {
		if rec := request("POST", tc.op, tc.body); rec.Code != 200 {
			t.Fatal(rec.Body.String())
		}
	}
	if rec := request("GET", "unknown", ""); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	for _, op := range []string{"state", "week"} {
		if rec := request("POST", op, `{}`); rec.Code != 405 {
			t.Fatal(rec.Code)
		}
	}
	if rec := request("GET", "seed", ""); rec.Code != 405 {
		t.Fatal(rec.Code)
	}
	// 错误路径不应改变承诺，State 的空集合始终是数组。
	rec = request("GET", "state", "")
	var state map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"projects", "exchanges", "commitments", "changesets", "days", "weeks", "merges", "reminders"} {
		if _, ok := state[key].([]any); !ok {
			t.Fatalf("%s is not an array", key)
		}
	}
}

func TestRejectOversize(t *testing.T) {
	fixtureStores(t, testRejectOversize)
}

func testRejectOversize(t *testing.T, s store.Store) {
	handler := fixture.Handler(s, fixture.Options{})
	body := `{"projects":[]}` + strings.Repeat(" ", 4<<20) + `{}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fixture/bl/u/seed", strings.NewReader(body)))
	if rec.Code != 400 {
		raw, _ := io.ReadAll(rec.Body)
		t.Fatalf("oversize: %d %s", rec.Code, raw)
	}
}

// 评测只取业务线处理进度；读取不推进进度，换绑后旧 Session 立即失效。
func TestCapturePosition(t *testing.T) {
	fixtureStores(t, testCapturePosition)
}

func testCapturePosition(t *testing.T, s store.Store) {
	handler := fixture.Handler(s, fixture.Options{})
	request := func(path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fixture/bl/u/"+path, strings.NewReader(body)))
		return rec
	}
	if rec := request("capture_position", `{"session":"s"}`); rec.Code != 400 {
		t.Fatal("unbound session accepted", rec.Code)
	}
	for _, tc := range []struct{ op, body string }{
		{"captures", `{"date":"2026-10-27","capture_id":"a","segments":[{"speaker":"wearer","text":"交付"}]}`},
		{"bind_day", `{"date":"2026-10-27","session":"s"}`},
	} {
		if rec := request(tc.op, tc.body); rec.Code != 200 {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	check := func(session, after string) {
		t.Helper()
		rec := request("capture_position", `{"session":"`+session+`"}`)
		var got map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 || len(got) != 1 || got["after"] != after {
			t.Fatal(rec.Code, rec.Body.String(), err)
		}
	}
	check("s", "0")
	if err := s.Update(context.Background(), "bl", "u", func(b *domain.Book) error {
		c := domain.NewCall(b, "s", nil, nil, time.Now())
		if _, err := c.Captures(domain.PageOptions{Limit: 200, MaxChars: 6000, GapMS: 300000, MaxToolResult: 8000}); err != nil {
			return err
		}
		c = domain.NewCall(b, "s", json.RawMessage(`{"source":{"from":"#1.0","to":"#1.0"}}`), nil, time.Now())
		c.FileChanges()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	check("s", "1.0")
	check("s", "1.0")
	if rec := request("bind_day", `{"date":"2026-10-27","session":"new"}`); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if rec := request("capture_position", `{"session":"s"}`); rec.Code != 400 {
		t.Fatal("old session accepted", rec.Code)
	}
	check("new", "1.0")
}

func TestCapturePreviewDoesNotChangeBook(t *testing.T) {
	fixtureStores(t, testCapturePreviewDoesNotChangeBook)
}

func testCapturePreviewDoesNotChangeBook(t *testing.T, s store.Store) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	if err := s.Update(context.Background(), "bl", "u", func(b *domain.Book) error {
		if err := b.BindDay("2026-10-27", "s"); err != nil {
			return err
		}
		_, err := b.AppendCaptures("2026-10-27", "a", []domain.CaptureSegment{{Speaker: "wearer", Text: "交付"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := func() []byte {
		t.Helper()
		var raw []byte
		if err := s.View(context.Background(), "bl", "u", func(b *domain.Book) error {
			var err error
			raw, err = json.Marshal(b)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := snapshot()
	handler := fixture.Handler(s, fixture.Options{Now: func() time.Time { return now }})
	for _, body := range []string{
		`{"session":"unbound","page_options":{"limit":200,"max_chars":6000,"gap_ms":300000,"max_tool_result":8000}}`,
		`{"session":"s","page_options":{"limit":0,"max_chars":6000,"gap_ms":300000,"max_tool_result":8000}}`,
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fixture/bl/u/capture_preview", strings.NewReader(body)))
		if rec.Code != 400 || !bytes.Equal(before, snapshot()) {
			t.Fatal("invalid preview accepted or changed book", rec.Code)
		}
	}
	for _, body := range []string{
		`{"session":"s","after":"new","page_options":{"limit":200,"max_chars":6000,"gap_ms":300000,"max_tool_result":8000}}`,
		`{"session":"s","all":true,"page_options":{"limit":200,"max_chars":6000,"gap_ms":600000,"max_tool_result":8000}}`,
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fixture/bl/u/capture_preview", strings.NewReader(body)))
		var page domain.StructuredPage
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || rec.Code != 200 || page.Kind != "page" || len(page.Segments) != 1 || page.Segments[0].Ref != "1.0" {
			t.Fatal(rec.Code, rec.Body.String(), err)
		}
		if !bytes.Equal(before, snapshot()) {
			t.Fatal("preview changed gap, read tail or processing position")
		}
	}
}
