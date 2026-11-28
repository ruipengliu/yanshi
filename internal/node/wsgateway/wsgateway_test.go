package wsgateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/node"
	"yanshi/internal/tracing"
)

func TestInvokeDeliveryTrace(t *testing.T) {
	tracer, recorder := tracing.NewRecorder()
	gw := &Gateway{Hub: &node.Hub{Tracer: tracer}}
	inv := &v1.Invoke{SessionId: "session", RunId: "run", CallId: "call", ArgumentsJson: `{"private":true}`}
	for _, sendErr := range []error{nil, context.Canceled} {
		err := gw.sendInvoke(t.Context(), func(_ context.Context, msg *v1.GatewayMessage) error {
			if msg.GetInvoke() != inv {
				t.Fatal("gateway changed the invocation")
			}
			return sendErr
		}, inv)
		if !errors.Is(err, sendErr) {
			t.Fatalf("sendInvoke returned %v, want %v", err, sendErr)
		}
	}
	records := recorder.Records()
	if len(records) != 2 {
		t.Fatalf("expected two deliveries, got %v", records)
	}
	for i, rec := range records {
		f := make(map[string]slog.Value)
		rec.Attrs(func(a slog.Attr) bool { f[a.Key] = a.Value; return true })
		if f[tracing.FieldName].String() != "node.deliver" || f[tracing.FieldTraceID].String() != tracing.TraceID("run") ||
			f[tracing.FieldSession].String() != "session" || f[tracing.FieldRun].String() != "run" ||
			f[tracing.FieldCall].String() != "call" || f[tracing.FieldOutcome].String() != []string{"ok", "canceled"}[i] ||
			f[tracing.FieldAttempt].Kind() != slog.KindAny {
			t.Errorf("delivery span = %v", f)
		}
	}
}

// TestHandshakesAreLimited：同时进行的握手达到上限时，新的接入在排队 HandshakeWait 后以 503 拒绝，并给出
// Retry-After；握手名额在连接完成握手或放弃后归还。
func TestHandshakesAreLimited(t *testing.T) {
	g := &Gateway{MaxHandshakes: 1, HandshakeWait: 50 * time.Millisecond}
	srv := httptest.NewServer(g)
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx := context.Background()

	// 第一条连接不发 Hello，一直占着唯一的握手名额。
	slow, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, resp, err := websocket.Dial(ctx, url, nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second handshake: resp %v, err %v; want 503", resp, err)
	}
	if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil || n < 2 || n > 10 {
		t.Fatalf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
	// 第一条连接放弃握手后名额归还：新的接入不再被拒绝（因为不发 Hello，之后由网关关闭）。
	slow.CloseNow()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, resp, err := websocket.Dial(ctx, url, nil)
		if err == nil {
			c.CloseNow()
			break
		}
		if resp == nil || resp.StatusCode != http.StatusServiceUnavailable || time.Now().After(deadline) {
			t.Fatalf("handshake slot not released: resp %v, err %v", resp, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
