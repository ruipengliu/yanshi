package nodesdk

import (
	"context"
	"errors"
	"strings"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
)

func text(s string) []*v1.ContentBlock {
	return []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: s}}}}
}

func resultText(r *v1.InvokeResult) string {
	var b strings.Builder
	for _, c := range r.GetContent() {
		b.WriteString(c.GetText().GetText())
	}
	return b.String()
}

func ledgers(t *testing.T) map[string]Ledger {
	return map[string]Ledger{"mem": NewMemLedger(), "file": FileLedger{Dir: t.TempDir()}}
}

func TestDuplicateDeliveryExecutesOnce(t *testing.T) {
	for name, l := range ledgers(t) {
		t.Run(name, func(t *testing.T) {
			n := 0
			e := NewExecutor(l, Capability{
				Spec:    &v1.CapabilitySpec{Name: "send"},
				Handler: func(context.Context, string) ([]*v1.ContentBlock, error) { n++; return text("ok"), nil },
			})
			inv := &v1.Invoke{CallId: "c1", Capability: "send"}
			for range 3 {
				r, err := e.Execute(context.Background(), inv)
				if err != nil || resultText(r) != "ok" {
					t.Fatalf("result %v err %v", r, err)
				}
			}
			if n != 1 {
				t.Fatalf("executed %d times", n)
			}
		})
	}
}

func TestCrashMidExecution(t *testing.T) {
	for name, l := range ledgers(t) {
		t.Run(name, func(t *testing.T) {
			// 模拟上次执行时进程被杀：ledger 停在 started。
			_ = l.Put("c1", &Record{State: StateStarted})
			_ = l.Put("c2", &Record{State: StateStarted})
			n := 0
			h := func(context.Context, string) ([]*v1.ContentBlock, error) { n++; return text("ok"), nil }
			e := NewExecutor(l,
				Capability{Spec: &v1.CapabilitySpec{Name: "send"}, Handler: h},
				Capability{Spec: &v1.CapabilitySpec{Name: "read", Idempotent: true}, Handler: h},
			)
			r, _ := e.Execute(context.Background(), &v1.Invoke{CallId: "c1", Capability: "send"})
			if !r.GetIsError() || !strings.HasPrefix(resultText(r), "outcome unknown") || n != 0 {
				t.Fatalf("non-idempotent retry: %v executed=%d", r, n)
			}
			r, _ = e.Execute(context.Background(), &v1.Invoke{CallId: "c2", Capability: "read"})
			if r.GetIsError() || n != 1 {
				t.Fatalf("idempotent retry: %v executed=%d", r, n)
			}
		})
	}
}

func TestCanceledExecutionStaysStarted(t *testing.T) {
	l := NewMemLedger()
	e := NewExecutor(l, Capability{
		Spec: &v1.CapabilitySpec{Name: "slow"},
		Handler: func(ctx context.Context, _ string) ([]*v1.ContentBlock, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Execute(ctx, &v1.Invoke{CallId: "c1", Capability: "slow"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if r, _ := l.Get("c1"); r.State != StateStarted {
		t.Fatalf("state = %v", r.State)
	}
}

func TestUnknownCapability(t *testing.T) {
	e := NewExecutor(NewMemLedger())
	r, err := e.Execute(context.Background(), &v1.Invoke{CallId: "c1", Capability: "nope"})
	if err != nil || !r.GetIsError() {
		t.Fatalf("r=%v err=%v", r, err)
	}
}

func TestFileLedgerRejectsUnsafeIDs(t *testing.T) {
	l := FileLedger{Dir: t.TempDir()}
	if err := l.Put("../etc", &Record{}); err == nil {
		t.Fatal("path traversal accepted")
	}
}
