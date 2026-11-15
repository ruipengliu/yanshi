package session_test

import (
	"reflect"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/session"
)

// 语音、压缩、审核等未登记事件不产生信封；快照恢复不改变选择结果。
func TestEnvelopeSelectionAfterSnapshot(t *testing.T) {
	st := &session.State{SessionID: "ses_v1_000_000_s"}
	for i, e := range richLog() {
		e.SessionId, e.Seq = "ses_v1_000_000_s", uint64(i+1)
		selected := session.SelectEnvelope(st, e)
		registered := false
		switch p := e.GetPayload().(type) {
		case *v1.Event_RunCompleted, *v1.Event_RunFailed, *v1.Event_ApprovalRequested, *v1.Event_SessionClosed, *v1.Event_BackgroundCancelled:
			registered = true
		case *v1.Event_RunRequested:
			registered = session.BackgroundInput(p.RunRequested.GetInput()) != nil
		case *v1.Event_Steered:
			registered = session.BackgroundInput(p.Steered.GetInput()) != nil
		}
		if (selected != nil) != registered {
			t.Fatalf("unexpected selection for %T", e.GetPayload())
		}
		if st.Created != nil {
			restored, err := session.FromSnapshot(st.Snapshot())
			if err != nil {
				t.Fatal(err)
			}
			if got := session.SelectEnvelope(restored, e); !reflect.DeepEqual(got, selected) {
				t.Fatalf("snapshot changed selection: %+v != %+v", got, selected)
			}
		}
		if err := st.Apply(e); err != nil {
			t.Fatal(err)
		}
	}
}
