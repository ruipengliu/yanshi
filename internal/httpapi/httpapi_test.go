package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/httpapi"
	"yanshi/internal/ids"
	"yanshi/internal/live"
	"yanshi/internal/model"
	"yanshi/internal/model/echo"
	"yanshi/internal/runtime"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue/memqueue"
)

// blocking 一直阻塞到 ctx 取消，用于验证中断会取消进行中的模型调用。
type blocking struct{ canceled chan struct{} }

func (b *blocking) Generate(ctx context.Context, _ *model.Request, onDelta func(model.Delta)) (*model.Response, error) {
	onDelta(model.Delta{Text: "thinking"})
	<-ctx.Done()
	close(b.canceled)
	return nil, ctx.Err()
}

func setup(t *testing.T) (*httptest.Server, *blocking) {
	t.Helper()
	agents, err := agentdef.NewRegistry(
		&agentdef.Def{Name: "echo", Version: "1", Model: "echo/any"},
		&agentdef.Def{Name: "slow", Version: "1", Model: "block/any"},
	)
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.Real{}
	store := &session.Store{Log: memlog.New(), IDs: ids.Random(), Clock: clk}
	queue := memqueue.New(clk)
	bus := live.NewMemBus()
	blk := &blocking{canceled: make(chan struct{})}
	gw := model.NewGateway()
	gw.Register("echo", echo.Provider{})
	gw.Register("block", blk)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, id := range []string{"w1", "w2"} {
		w := runtime.New(runtime.Config{ID: id, Store: store, Queue: queue, Agents: agents, Model: gw,
			Catalog: &capability.Catalog{Local: capability.NewRegistry()}, Live: bus, IdleWait: 5 * time.Millisecond})
		go w.Run(ctx)
	}
	svc := &service.Service{Store: store, Queue: queue, Agents: agents}
	srv := httptest.NewServer((&httpapi.Server{Service: svc, Live: bus}).Handler())
	t.Cleanup(srv.Close)
	return srv, blk
}

func post(t *testing.T, url string, body any, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func createSession(t *testing.T, srv *httptest.Server, agent string) string {
	var res struct {
		SessionID string `json:"session_id"`
	}
	if code := post(t, srv.URL+"/v1/sessions", map[string]string{"business_line": "bl", "end_user": "u", "agent": agent}, &res); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	return res.SessionID
}

type sseItem struct {
	kind  string
	id    string
	event *v1.Event
	delta string
}

// readSSE 读取流直到 stop 返回 true。
func readSSE(t *testing.T, url string, header http.Header, stop func(sseItem) bool) []sseItem {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var items []sseItem
	var cur sseItem
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.kind = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			data := []byte(strings.TrimPrefix(line, "data: "))
			if cur.kind == "event" {
				cur.event = &v1.Event{}
				if err := protojson.Unmarshal(data, cur.event); err != nil {
					t.Fatal(err)
				}
			} else {
				var d live.Delta
				_ = json.Unmarshal(data, &d)
				cur.delta = d.Text
			}
		case line == "":
			if cur.kind != "" {
				items = append(items, cur)
				if stop(cur) {
					return items
				}
			}
			cur = sseItem{}
		}
	}
	t.Fatalf("stream ended before stop condition: %v", sc.Err())
	return nil
}

func TestRunStreamsAndCompletes(t *testing.T) {
	srv, _ := setup(t)
	sid := createSession(t, srv, "echo")
	var sub struct {
		RunID   string `json:"run_id"`
		Steered bool   `json:"steered"`
	}
	if code := post(t, srv.URL+"/v1/sessions/"+sid+"/inputs", map[string]string{"text": "你好"}, &sub); code != http.StatusAccepted || sub.Steered {
		t.Fatalf("submit: %d %+v", code, sub)
	}
	items := readSSE(t, srv.URL+"/v1/sessions/"+sid+"/stream", nil, func(i sseItem) bool {
		return i.event.GetRunCompleted() != nil
	})
	var text string
	for _, i := range items {
		if m := i.event.GetAssistantMessage(); m != nil {
			text = model.Text(m.GetContent())
		}
	}
	if text != "echo: 你好" {
		t.Fatalf("assistant text = %q", text)
	}

	// 断线续传：从第 2 个事件之后继续，必须恰好补齐其余已提交事件。
	resumed := readSSE(t, srv.URL+"/v1/sessions/"+sid+"/stream", http.Header{"Last-Event-ID": {"2"}}, func(i sseItem) bool {
		return i.event.GetRunCompleted() != nil
	})
	if first := resumed[0]; first.event.GetSeq() != 3 {
		t.Fatalf("resumed stream starts at seq %d, want 3", first.event.GetSeq())
	}
}

func TestInterruptCancelsInFlightModelCall(t *testing.T) {
	srv, blk := setup(t)
	sid := createSession(t, srv, "slow")
	var sub struct {
		RunID string `json:"run_id"`
	}
	post(t, srv.URL+"/v1/sessions/"+sid+"/inputs", map[string]string{"text": "hi"}, &sub)

	readSSE(t, srv.URL+"/v1/sessions/"+sid+"/stream", nil, func(i sseItem) bool { return i.delta == "thinking" })
	if code := post(t, srv.URL+"/v1/sessions/"+sid+"/runs/"+sub.RunID+"/interrupt", struct{}{}, nil); code != http.StatusNoContent {
		t.Fatalf("interrupt: %d", code)
	}
	select {
	case <-blk.canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("model call was not canceled after interrupt")
	}

	var st struct {
		Runs []struct {
			Status string `json:"status"`
		} `json:"runs"`
	}
	resp, err := http.Get(srv.URL + "/v1/sessions/" + sid)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_ = json.NewDecoder(resp.Body).Decode(&st)
	if len(st.Runs) != 1 || st.Runs[0].Status != "interrupted" {
		t.Fatalf("runs = %+v", st.Runs)
	}
}

func TestErrors(t *testing.T) {
	srv, _ := setup(t)
	if code := post(t, srv.URL+"/v1/sessions", map[string]string{"business_line": "bl", "end_user": "u", "agent": "nope"}, nil); code != http.StatusBadRequest {
		t.Fatalf("unknown agent: %d", code)
	}
	if code := post(t, srv.URL+"/v1/sessions/missing/inputs", map[string]string{"text": "x"}, nil); code != http.StatusNotFound {
		t.Fatalf("missing session: %d", code)
	}
}
