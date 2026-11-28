package e2e

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/artifact"
	"yanshi/internal/askuser"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/model"
	"yanshi/internal/model/script"
	"yanshi/internal/node"
	"yanshi/internal/node/wsgateway"
	"yanshi/internal/runtime"
	"yanshi/internal/sandbox"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/tracing"
	"yanshi/internal/workqueue"
)

func TestTracingAcrossRoles(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	agents, err := agentdef.NewRegistry(
		&agentdef.Def{Name: "parent", Version: "1", Model: "script/any",
			Capabilities: []string{"device:*", "sandbox:*", "agent:helper", "memory_search", "mcp:fixture/*", "ask_user"}},
		&agentdef.Def{Name: "helper", Version: "1", Model: "script/any"},
	)
	if err != nil {
		t.Fatal(err)
	}
	tracer, recorder := tracing.NewRecorder()
	clk := clock.Real{}
	stores := app.Memory(clk)
	models := model.NewGateway()
	models.Register("script", script.Provider{})
	opts := app.Options{Clock: clk, Agents: agents, Model: models, Blobs: artifact.NewMemBlobs(),
		NodeAuth: node.InsecureDevAuth{}, SandboxTools: true, MCP: traceMCPSource{}, DefaultTimeout: time.Minute, Tracer: tracer}
	gateway := app.NewCore(stores, opts)
	worker := runtime.New(app.NewCore(stores, opts).WorkerConfig("worker-only", workqueue.Pool{}, nil))
	controller := app.NewCore(stores, opts).ControllerConfig("sandbox-only", sandbox.NewFake(), nil)
	controller.IdleWait = 5 * time.Millisecond
	var roles sync.WaitGroup
	roles.Go(func() { worker.Run(ctx) })
	roles.Go(func() { controller.Run(ctx) })
	defer func() { cancel(); roles.Wait() }()

	// 用内存连接承载真实的 HTTP 升级和 Connection 帧，以便在没有监听权限的环境中验证网关。
	listener := &traceListener{pending: make(chan net.Conn), closed: make(chan struct{})}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: &wsgateway.Gateway{Hub: gateway.Hub}}}
	server.Start()
	defer server.Close()
	transport := &http.Transport{DialContext: listener.dial}
	defer transport.CloseIdleConnections()
	dial := func() *websocket.Conn {
		t.Helper()
		conn, _, err := websocket.Dial(ctx, "ws://gateway/v1/connect", &websocket.DialOptions{
			HTTPClient: &http.Client{Transport: transport},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.CloseNow() })
		writeTraceNode(t, ctx, conn, &v1.NodeMessage{Msg: &v1.NodeMessage_Hello{Hello: &v1.Hello{
			NodeId: "device", Label: "phone", BusinessLine: "bl", EndUser: "u",
			Capabilities: []*v1.CapabilitySpec{{Name: "probe", Idempotent: true, Risk: v1.Risk_RISK_LOW}},
		}}})
		if msg := readTraceNode(t, ctx, conn); msg.GetWelcome() == nil {
			t.Fatalf("expected Welcome, got %v", msg)
		}
		return conn
	}
	device := dial()
	sid, err := gateway.Service.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := gateway.Service.Submit(ctx, sid, model.TextBlocks(
		"call phone__probe {}\ncall sandbox__run_python {\"code\":\"print(42)\"}\ncall agent__helper {\"task\":\"echo\"}\n"+
			"call memory_search {\"query\":\"note\"}\ncall mcp_fixture__note {}\nask Continue?"))
	if err != nil {
		t.Fatal(err)
	}
	first := readTraceNode(t, ctx, device).GetInvoke()
	if first.GetRunId() != res.RunID || first.GetSessionId() != sid {
		t.Fatalf("first Invoke = %v", first)
	}
	traceID := tracing.TraceID(res.RunID)
	// 收完帧时发送方仍可能在返回，等发送记录完成后再模拟断线。
	awaitTrace(t, ctx, recorder, traceID, "node.deliver", "")
	_ = device.CloseNow()
	device = dial()
	replay := readTraceNode(t, ctx, device).GetInvoke()
	if !proto.Equal(first, replay) {
		t.Fatalf("redelivered Invoke = %v, want %v", replay, first)
	}
	writeTraceNode(t, ctx, device, &v1.NodeMessage{Msg: &v1.NodeMessage_Result{Result: &v1.InvokeResult{
		CallId: replay.GetCallId(), Content: model.TextBlocks("device result"),
	}}})
	for {
		if msg := readTraceNode(t, ctx, device); msg.GetResultAck().GetCallId() == first.GetCallId() {
			break
		}
	}
	defer device.CloseNow()

	question := awaitTrace(t, ctx, recorder, traceID, "tool.call", "ask_user")
	if err := gateway.Service.Answer(ctx, sid, traceFields(question[len(question)-1])[tracing.FieldCall].String(),
		&askuser.Answer{Text: "yes"}); err != nil {
		t.Fatal(err)
	}
	records := awaitTrace(t, ctx, recorder, traceID, "run.end", "")
	st, err := gateway.Service.Load(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	parent := st.Run(res.RunID)
	if parent.Status != session.RunCompleted || len(parent.Calls) != 6 {
		t.Fatalf("parent Run status %v, calls %d", parent.Status, len(parent.Calls))
	}
	checkRoleTrace(t, records, sid, traceID, parent)
}

func awaitTrace(t *testing.T, ctx context.Context, recorder *tracing.Recorder, traceID, name, kind string) []slog.Record {
	t.Helper()
	for {
		records := recorder.Records()
		for i, rec := range records {
			f := traceFields(rec)
			if f[tracing.FieldName].String() == name && f[tracing.FieldTraceID].String() == traceID &&
				(kind == "" || f[tracing.FieldKind].String() == kind) {
				return records[:i+1]
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("missing span %s in trace %s, spans = %v", name, traceID, records)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func checkRoleTrace(t *testing.T, records []slog.Record, sid, traceID string, parent *session.Run) {
	t.Helper()
	type key struct{ name, kind string }
	want := map[key]string{
		{"run.submit", ""}: "ok", {"queue.claim", ""}: "ok", {"run.attempt", ""}: "", {"model.call", ""}: "ok",
		{"tool.call", "node"}: "dispatched", {"tool.call", "sandbox"}: "dispatched", {"tool.call", "delegation"}: "dispatched",
		{"tool.call", "local"}: "ok", {"tool.call", "mcp"}: "ok", {"tool.call", "ask_user"}: "dispatched",
		{"node.deliver", ""}: "ok", {"sandbox.exec", "sandbox"}: "ok",
		{"sandbox.prepare", "sandbox"}: "ok", {"sandbox.run", "sandbox"}: "ok",
		{"tool.result", "node"}: "ok", {"tool.result", "sandbox"}: "ok", {"tool.result", "delegation"}: "ok",
		{"run.end", ""}: "done",
	}
	calls := map[string]string{
		"node": parent.Calls[0].Call.GetCallId(), "sandbox": parent.Calls[1].Call.GetCallId(), "delegation": parent.Calls[2].Call.GetCallId(),
		"local": parent.Calls[3].Call.GetCallId(), "mcp": parent.Calls[4].Call.GetCallId(), "ask_user": parent.Calls[5].Call.GetCallId(),
	}
	seen := make(map[key]int)
	attempts := make(map[string]uint64)
	childTrace, childResultLink, sandboxExec := "", "", ""
	var children []map[string]slog.Value
	for _, rec := range records {
		f := traceFields(rec)
		k := key{name: f[tracing.FieldName].String()}
		if kind, ok := f[tracing.FieldKind]; ok {
			k.kind = kind.String()
		}
		gotTrace := f[tracing.FieldTraceID].String()
		if k.name == "run.submit" && f[tracing.FieldLinkTraceID].String() == traceID {
			childTrace = gotTrace
			if childTrace == traceID || childTrace != tracing.TraceID(f[tracing.FieldRun].String()) || f[tracing.FieldSession].String() == sid {
				t.Fatalf("child submit = %v", f)
			}
		}
		if k.name == "queue.claim" {
			if f[tracing.FieldSession].String() != sid {
				continue
			}
		} else if gotTrace != traceID {
			continue
		} else if f[tracing.FieldSession].String() != sid || f[tracing.FieldRun].String() != parent.ID {
			t.Fatalf("wrong identity for span %v", f)
		}
		if outcome, ok := want[k]; ok {
			if outcome != "" && f[tracing.FieldOutcome].String() != outcome {
				t.Errorf("outcome for %v = %v, want %s", k, f, outcome)
			}
			seen[k]++
		}
		if callID := calls[k.kind]; callID != "" && f[tracing.FieldCall].String() != callID {
			t.Errorf("wrong call ID for %v: %v", k, f)
		}
		switch k.name {
		case "run.attempt":
			attempts[f[tracing.FieldSpanID].String()] = f[tracing.FieldAttempt].Uint64()
		case "tool.call", "sandbox.prepare", "sandbox.run":
			children = append(children, f)
		case "node.deliver":
			if f[tracing.FieldCall].String() != calls["node"] || f[tracing.FieldAttempt].Kind() != slog.KindAny {
				t.Errorf("device delivery attributes = %v", f)
			}
		case "sandbox.exec":
			sandboxExec = f[tracing.FieldSpanID].String()
		case "tool.result":
			switch k.kind {
			case "node":
				if f[tracing.FieldStart].String() != parent.Calls[0].DispatchedAt.UTC().Format(time.RFC3339Nano) {
					t.Errorf("device result did not start at persisted dispatch: %v", f)
				}
			case "delegation":
				childResultLink = f[tracing.FieldLinkTraceID].String()
			}
		}
	}
	for k := range want {
		if seen[k] == 0 {
			t.Errorf("missing span %v in parent trace %s", k, traceID)
		}
	}
	if seen[key{"node.deliver", ""}] != 2 || childTrace == "" || childResultLink != childTrace {
		t.Errorf("delivery spans %d, child trace %q, delegation result link %q", seen[key{"node.deliver", ""}], childTrace, childResultLink)
	}
	for _, f := range children {
		parentSpan := f[tracing.FieldParentSpanID].String()
		if f[tracing.FieldName].String() == "tool.call" {
			if attempt := attempts[parentSpan]; attempt == 0 || f[tracing.FieldAttempt].Uint64() != attempt {
				t.Errorf("tool call has wrong Attempt: %v", f)
			}
		} else if parentSpan != sandboxExec {
			t.Errorf("sandbox child has wrong parent: %v", f)
		}
	}
}

type traceMCPSource struct{}

func (traceMCPSource) Tools(context.Context, string, []string) ([]capability.Tool, error) {
	cp := capability.Func{
		S: capability.Spec{Name: "mcp_fixture__note", ReadOnly: true, Risk: v1.Risk_RISK_LOW},
		Fn: func(context.Context, capability.Invocation) ([]*v1.ContentBlock, error) {
			return model.TextBlocks("MCP result"), nil
		},
	}
	return []capability.Tool{{Spec: cp.Spec(), Local: cp}}, nil
}

func traceFields(rec slog.Record) map[string]slog.Value {
	f := make(map[string]slog.Value)
	rec.Attrs(func(a slog.Attr) bool { f[a.Key] = a.Value; return true })
	return f
}

func writeTraceNode(t *testing.T, ctx context.Context, conn *websocket.Conn, msg *v1.NodeMessage) {
	t.Helper()
	b, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageBinary, b); err != nil {
		t.Fatal(err)
	}
}

func readTraceNode(t *testing.T, ctx context.Context, conn *websocket.Conn) *v1.GatewayMessage {
	t.Helper()
	_, b, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	msg := &v1.GatewayMessage{}
	if err := proto.Unmarshal(b, msg); err != nil {
		t.Fatal(err)
	}
	return msg
}

type traceListener struct {
	pending chan net.Conn
	closed  chan struct{}
	once    sync.Once
}

func (l *traceListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.pending:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *traceListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (*traceListener) Addr() net.Addr { return &net.TCPAddr{Port: 80} }

func (l *traceListener) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	local, remote := net.Pipe()
	var err error
	select {
	case l.pending <- remote:
		return local, nil
	case <-l.closed:
		err = net.ErrClosed
	case <-ctx.Done():
		err = ctx.Err()
	}
	_ = local.Close()
	_ = remote.Close()
	return nil, err
}
