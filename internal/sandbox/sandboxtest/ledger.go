package sandboxtest

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/sandbox"
	"yanshi/sdk/nodesdk"
)

// RunLedger 沿用 SDK 账本的状态、结果、删除与保留期契约，并验证真实 Session 隔离。
func RunLedger(t *testing.T, newLedger func(*testing.T) sandbox.Ledger) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	get := func(t *testing.T, l sandbox.Ledger, sid, call string) *nodesdk.Record {
		t.Helper()
		r, err := l.Get(sid, call)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	t.Run("StateAndSessionIsolation", func(t *testing.T) {
		l := newLedger(t)
		if r := get(t, l, "ses_v1_000_000_s1", "c"); r.State != nodesdk.StateNew || r.Result != nil {
			t.Fatalf("missing: %+v", r)
		}
		for _, sid := range []string{"ses_v1_000_000_s1", "ses_v1_000_000_s2"} {
			if err := l.Put(sid, "c", &nodesdk.Record{SessionID: sid, State: nodesdk.StateStarted, UpdatedAt: t0}); err != nil {
				t.Fatal(err)
			}
		}
		result := &v1.InvokeResult{CallId: "c", IsError: true, Content: []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: "result"}}}}}
		if err := l.Put("ses_v1_000_000_s1", "c", &nodesdk.Record{SessionID: "ses_v1_000_000_s1", State: nodesdk.StateDone, UpdatedAt: t0, Result: result}); err != nil {
			t.Fatal(err)
		}
		if r := get(t, l, "ses_v1_000_000_s1", "c"); r.State != nodesdk.StateDone || r.SessionID != "ses_v1_000_000_s1" || !r.UpdatedAt.Equal(t0) || !r.Result.GetIsError() || r.Result.GetContent()[0].GetText().GetText() != "result" {
			t.Fatalf("result: %+v", r)
		}
		if r := get(t, l, "ses_v1_000_000_s2", "c"); r.State != nodesdk.StateStarted || r.Result != nil {
			t.Fatalf("another session overwritten: %+v", r)
		}
		if r := get(t, l, "ses_v1_000_000_s1", "other"); r.State != nodesdk.StateNew {
			t.Fatal("another call overwritten")
		}
		if err := l.Put("ses_v1_000_000_s1", "bad", &nodesdk.Record{SessionID: "ses_v1_000_000_s2", State: nodesdk.StateStarted}); err == nil {
			t.Fatal("accepted another session's record")
		}
		if err := sandbox.BindLedger(l, "ses_v1_000_000_s1").Forget("ses_v1_000_000_s2"); err == nil {
			t.Fatal("bound ledger forgot another session")
		}
	})
	t.Run("ForgetAndPrune", func(t *testing.T) {
		l := newLedger(t)
		for _, sid := range []string{"ses_v1_000_000_s1", "ses_v1_000_000_s2"} {
			for i, call := range []string{"old", "new"} {
				if err := l.Put(sid, call, &nodesdk.Record{SessionID: sid, State: nodesdk.StateStarted, UpdatedAt: t0.Add(time.Duration(i) * 48 * time.Hour)}); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := l.Forget("ses_v1_000_000_s1"); err != nil {
			t.Fatal(err)
		}
		if exists, err := l.HasSession("ses_v1_000_000_s1"); err != nil || exists {
			t.Fatalf("forgotten session still present: %v, %v", exists, err)
		}
		if exists, err := l.HasSession("ses_v1_000_000_s2"); err != nil || !exists {
			t.Fatalf("other session disappeared: %v, %v", exists, err)
		}
		if err := l.Prune(t0.Add(24 * time.Hour)); err != nil {
			t.Fatal(err)
		}
		for _, sid := range []string{"ses_v1_000_000_s1", "ses_v1_000_000_s2"} {
			for _, call := range []string{"old", "new"} {
				want := nodesdk.StateNew
				if sid == "ses_v1_000_000_s2" && call == "new" {
					want = nodesdk.StateStarted
				}
				if r := get(t, l, sid, call); r.State != want {
					t.Fatalf("%s/%s: state %v, want %v", sid, call, r.State, want)
				}
			}
		}
		if err := l.Prune(t0.Add(72 * time.Hour)); err != nil {
			t.Fatal(err)
		}
		if exists, err := l.HasSession("ses_v1_000_000_s2"); err != nil || exists {
			t.Fatalf("pruned session still present: %v, %v", exists, err)
		}
	})
	t.Run("BoundExecutorPersistsBeforeExecutionAndReplays", func(t *testing.T) {
		l := newLedger(t)
		invoked := 0
		cap := nodesdk.Capability{Spec: &v1.CapabilitySpec{Name: "execute"}, Handler: func(ctx context.Context, _ string) ([]*v1.ContentBlock, error) {
			inv := nodesdk.Invocation(ctx)
			if r := get(t, l, inv.GetSessionId(), inv.GetCallId()); r.State != nodesdk.StateStarted {
				t.Fatal("handler ran before started was persisted")
			}
			invoked++
			return nil, nil
		}}
		for _, sid := range []string{"ses_v1_000_000_s1", "ses_v1_000_000_s2"} {
			for range 2 {
				// 新 Executor 模拟控制器接管；同一外部 ID 在两个 Session 各执行一次。
				e := nodesdk.NewExecutor(sandbox.BindLedger(l, sid), cap)
				if _, err := e.Execute(context.Background(), &v1.Invoke{SessionId: sid, CallId: "same", Capability: "execute"}); err != nil {
					t.Fatal(err)
				}
			}
		}
		if invoked != 2 {
			t.Fatalf("executed %d times, want once per session", invoked)
		}
		if err := l.Put("ses_v1_000_000_s1", "crashed", &nodesdk.Record{SessionID: "ses_v1_000_000_s1", State: nodesdk.StateStarted, UpdatedAt: t0}); err != nil {
			t.Fatal(err)
		}
		e := nodesdk.NewExecutor(sandbox.BindLedger(l, "ses_v1_000_000_s1"), cap)
		res, err := e.Execute(context.Background(), &v1.Invoke{SessionId: "ses_v1_000_000_s1", CallId: "crashed", Capability: "execute"})
		if err != nil || !res.GetIsError() || invoked != 2 {
			t.Fatalf("started call executed again: %v, %v, %d", res, err, invoked)
		}
		e = nodesdk.NewExecutor(sandbox.BindLedger(failPut{Ledger: l}, "ses_v1_000_000_s1"), cap)
		if _, err := e.Execute(context.Background(), &v1.Invoke{SessionId: "ses_v1_000_000_s1", CallId: "failed", Capability: "execute"}); err == nil || invoked != 2 {
			t.Fatal("handler ran after started write failed")
		}
	})
}

type failPut struct{ sandbox.Ledger }

func (failPut) Put(string, string, *nodesdk.Record) error { return errors.New("write failed") }
