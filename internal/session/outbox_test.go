package session

import (
	"context"
	"reflect"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/askuser"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/outbox"
	"yanshi/internal/outbox/memoutbox"
	"yanshi/internal/workqueue/memqueue"
)

func TestSelectEnvelope(t *testing.T) {
	base := func(tail ...*v1.Event) []*v1.Event {
		c := created()
		c.GetSessionCreated().Agent = &v1.AgentRef{Name: "a", Version: "1"}
		return append([]*v1.Event{c, requested("r1"), attempt("r1", 1)}, tail...)
	}
	ask := func() *v1.Event {
		e := assistant("r1", 1, "q")
		e.GetAssistantMessage().ToolCalls[0].Capability = askuser.Capability
		return e
	}
	cases := []struct {
		name      string
		prefix    []*v1.Event
		event     *v1.Event
		kind      outbox.Kind
		run, call string
	}{
		{"completed", base(), completed("r1", 1), outbox.RunCompleted, "r1", ""},
		{"failed", base(), failed("r1", 1), outbox.RunFailed, "r1", ""},
		{"question", base(ask()), routed("r1", 1, "q", askuser.NodeID), outbox.Question, "r1", "q"},
		{"approval", base(assistant("r1", 1, "c")), approvalReq("r1", 1, "c"), outbox.ApprovalRequested, "r1", "c"},
		{"closed", base(completed("r1", 1)), closed(), outbox.SessionClosed, "", ""},
		{"background result requests", bgPrefix(), bgRequested("r2", bgBlock("c1", BackgroundReasonResult)), outbox.BackgroundSettled, "r1", "c1"},
		{"background timeout requests", bgPrefix(), bgRequested("r2", bgBlock("c1", BackgroundReasonTimeout)), outbox.BackgroundSettled, "r1", "c1"},
		{"background result steers", bgPrefix(requested("r2")), bgSteered("r2", bgBlock("c2", BackgroundReasonResult)), outbox.BackgroundSettled, "r1", "c2"},
		{"background timeout steers", bgPrefix(requested("r2")), bgSteered("r2", bgBlock("c2", BackgroundReasonTimeout)), outbox.BackgroundSettled, "r1", "c2"},
		{"background cancelled", bgPrefix(), bgCancelled("c3"), outbox.BackgroundSettled, "r1", "c3"},
		{"takeover does not ask again", base(ask(), routed("r1", 1, "q", askuser.NodeID), attempt("r1", 2)), routed("r1", 2, "q", askuser.NodeID), "", "", ""},
		{"other capability", base(assistant("r1", 1, "c")), routed("r1", 1, "c", "phone"), "", "", ""},
		{"wrong question route", base(ask()), routed("r1", 1, "q", "phone"), "", "", ""},
		{"unstarted question", base(), ask(), "", "", ""},
		{"backgrounding is not settling", base(assistant("r1", 1, "c"), routed("r1", 1, "c", "phone")), backgrounded("r1", 1, "c", "phone"), "", "", ""},
		{"ordinary result", base(assistant("r1", 1, "c"), started("r1", 1, "c")), result("r1", 1, "c"), "", "", ""},
		{"ordinary input", base(completed("r1", 1)), requested("r2"), "", "", ""},
		{"ordinary steering", base(), steered("r1"), "", "", ""},
		{"interrupted", base(), interrupted("r1"), "", "", ""},
		{"suspended", base(), suspended("r1", 1), "", "", ""},
		{"approval decided", base(assistant("r1", 1, "c"), approvalReq("r1", 1, "c")), decided("r1", "c", true), "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := Reduce(build(tc.prefix...))
			if err != nil {
				t.Fatal(err)
			}
			tc.event.SessionId, tc.event.Seq = "s", st.Seq+1
			if err := st.Check(tc.event); err != nil {
				t.Fatal(err)
			}
			before := st.Clone()
			got := SelectEnvelope(st, tc.event)
			if !Equal(before, st) {
				t.Fatal("selector mutated projection")
			}
			if tc.kind == "" {
				if got != nil {
					t.Fatalf("unexpected envelope: %+v", got)
				}
				return
			}
			want := &outbox.Envelope{EnvelopeID: outbox.ID("s", tc.event.Seq, tc.kind), BusinessLine: "bl", EndUser: "u", SessionID: "s",
				Seq: tc.event.Seq, Kind: tc.kind, RunID: tc.run, CallID: tc.call, Agent: st.Agent.GetName(), AgentVersion: st.Agent.GetVersion()}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("envelope=%+v want=%+v", got, want)
			}
		})
	}
}

// 同批创建、切版本、结束多个 Run：必须按每个事件之前的投影选择，不能都套最终版本。
func TestCommitEnvelopeUsesPerEventAgent(t *testing.T) {
	for _, wake := range []bool{false, true} {
		o := memoutbox.New()
		log := memlog.NewWithOutbox(o)
		q := memqueue.New(clock.Real{})
		s := &Store{Log: log, Waking: log.Waking(q), Outbox: o, Clock: clock.Real{}, IDs: ids.Sequential("e")}
		st := &State{SessionID: "s"}
		c := created()
		c.GetSessionCreated().Agent = &v1.AgentRef{Name: "a", Version: "1"}
		switchAgent := &v1.Event{Payload: &v1.Event_AgentSwitched{AgentSwitched: &v1.AgentSwitched{
			From: &v1.AgentRef{Name: "a", Version: "1"}, To: &v1.AgentRef{Name: "a", Version: "2"}}}}
		events := []*v1.Event{c, requested("r1"), attempt("r1", 1), completed("r1", 1), switchAgent,
			requested("r2"), attempt("r2", 1), failed("r2", 1), closed()}
		err := s.Update(context.Background(), st, func(*State) (Decision, error) { return Decision{Events: events, Wake: wake}, nil })
		if err != nil {
			t.Fatal(err)
		}
		got, err := o.List(context.Background(), "bl")
		if err != nil || len(got) != 3 {
			t.Fatalf("got=%+v err=%v", got, err)
		}
		if got[0].AgentVersion != "1" || got[1].AgentVersion != "2" || got[2].AgentVersion != "2" {
			t.Fatalf("wrong versions: %+v", got)
		}
		if got[0].Kind != outbox.RunCompleted || got[1].Kind != outbox.RunFailed || got[2].Kind != outbox.SessionClosed {
			t.Fatalf("wrong kinds: %+v", got)
		}
	}
}

func TestInvalidBatchLeavesNoEnvelopes(t *testing.T) {
	o := memoutbox.New()
	log := memlog.NewWithOutbox(o)
	s := &Store{Log: log, Outbox: o, Clock: clock.Real{}, IDs: ids.Sequential("e")}
	st := &State{SessionID: "s"}
	err := s.Commit(context.Background(), st, created(), requested("r1"), attempt("r1", 1), completed("r1", 1), failed("r1", 1))
	if err == nil {
		t.Fatal("invalid batch accepted")
	}
	rows, err := o.List(context.Background(), "bl")
	if err != nil || len(rows) != 0 || st.Seq != 0 {
		t.Fatalf("invalid batch leaked: %+v %v", rows, err)
	}
}
