// Package ledgertest 是 nodesdk.Ledger 实现的一致性测试套件。
package ledgertest

import (
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/sdk/nodesdk"
)

func Run(t *testing.T, newLedger func(t *testing.T) nodesdk.Ledger) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t.Run("ForgetAndPrune", func(t *testing.T) {
		l := newLedger(t)
		put := func(id, sid string, at time.Time) {
			if err := l.Put(id, &nodesdk.Record{State: nodesdk.StateDone, Result: &v1.InvokeResult{CallId: id}, SessionID: sid, UpdatedAt: at}); err != nil {
				t.Fatal(err)
			}
		}
		put("c1", "s1", t0)
		put("c2", "s1", t0.Add(48*time.Hour))
		put("c3", "s2", t0)
		put("c4", "s2", t0.Add(48*time.Hour))
		if r, _ := l.Get("c2"); r.SessionID != "s1" || !r.UpdatedAt.Equal(t0.Add(48*time.Hour)) {
			t.Fatalf("fields not persisted: %+v", r)
		}
		if err := l.Forget("s1"); err != nil {
			t.Fatal(err)
		}
		if err := l.Prune(t0.Add(24 * time.Hour)); err != nil {
			t.Fatal(err)
		}
		for id, want := range map[string]nodesdk.State{"c1": nodesdk.StateNew, "c2": nodesdk.StateNew, "c3": nodesdk.StateNew, "c4": nodesdk.StateDone} {
			if r, _ := l.Get(id); r.State != want {
				t.Errorf("%s: state = %v, want %v", id, r.State, want)
			}
		}
	})

	t.Run("MissingIsNew", func(t *testing.T) {
		r, err := newLedger(t).Get("c1")
		if err != nil || r.State != nodesdk.StateNew || r.Result != nil {
			t.Fatalf("get = %+v, %v", r, err)
		}
	})
	t.Run("StateTransitionsAndResult", func(t *testing.T) {
		l := newLedger(t)
		if err := l.Put("c1", &nodesdk.Record{State: nodesdk.StateStarted}); err != nil {
			t.Fatal(err)
		}
		if r, _ := l.Get("c1"); r.State != nodesdk.StateStarted {
			t.Fatalf("state = %v", r.State)
		}
		res := &v1.InvokeResult{CallId: "c1", IsError: true, Content: []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: "x"}}}}}
		if err := l.Put("c1", &nodesdk.Record{State: nodesdk.StateDone, Result: res}); err != nil {
			t.Fatal(err)
		}
		r, err := l.Get("c1")
		if err != nil || r.State != nodesdk.StateDone || !r.Result.GetIsError() || r.Result.GetContent()[0].GetText().GetText() != "x" {
			t.Fatalf("get = %+v, %v", r, err)
		}
		if r2, _ := l.Get("c2"); r2.State != nodesdk.StateNew {
			t.Fatal("records leak across call ids")
		}
	})
}
