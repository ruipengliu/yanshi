// Package ledgertest 是 nodesdk.Ledger 实现的一致性测试套件。
package ledgertest

import (
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/sdk/nodesdk"
)

func Run(t *testing.T, newLedger func(t *testing.T) nodesdk.Ledger) {
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
