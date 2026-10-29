package recovery_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"xinli/recovery"
)

func TestReplayAllKindsAcrossCheckpoints(t *testing.T) {
	payloads := []string{
		`"session_created":{"business_line":"bl","end_user":"u","agent":{"name":"a","version":"1"}}`,
		`"assistant_message":{"run_id":"r","tool_calls":[{"call_id":"q","capability":"ask_user"},{"call_id":"x","capability":"other"}],"content":[{"text":{"text":"private"}}]}`,
		`"tool_call_started":{"run_id":"r","call_id":"x","node_id":"@user"}`,
		`"tool_call_started":{"run_id":"r","call_id":"q","node_id":"@user"}`,
		`"tool_call_started":{"run_id":"r","call_id":"q","node_id":"@user"}`,
		`"approval_requested":{"run_id":"r","call_id":"c"}`,
		`"tool_result":{"run_id":"r","call_id":"b","backgrounded":{}}`,
		`"run_completed":{"run_id":"r"}`,
		`"agent_switched":{"to":{"name":"a","version":"2"}}`,
		`"run_requested":{"run_id":"new","input":[{"background_result":{"call_id":"b"}}]}`,
		`"tool_result":{"run_id":"new","call_id":"b2","backgrounded":{}}`,
		`"steered":{"run_id":"new","input":[{"background_result":{"call_id":"b2"}}]}`,
		`"tool_result":{"run_id":"new","call_id":"b3","backgrounded":{}}`,
		`"background_cancelled":{"call_id":"b3"}`,
		`"run_failed":{"run_id":"new"}`,
		`"session_closed":{}`,
	}
	cp := recovery.Checkpoint{SessionID: "s", EndUser: "u"}
	kinds := map[uint64]string{4: "question", 6: "approval_requested", 8: "run_completed", 10: "background_settled", 12: "background_settled", 14: "background_settled", 15: "run_failed", 16: "session_closed"}
	for i, payload := range payloads {
		raw := json.RawMessage(fmt.Sprintf(`{"session_id":"s","seq":"%d",%s}`, i+1, payload))
		envelopes, err := recovery.Replay("bl", &cp, []json.RawMessage{raw})
		if err != nil {
			t.Fatal(err)
		}
		want := kinds[uint64(i+1)]
		if want == "" {
			if len(envelopes) != 0 {
				t.Fatalf("unexpected envelope at %d", i+1)
			}
		} else {
			if len(envelopes) != 1 || envelopes[0].Kind != want || envelopes[0].Seq != uint64(i+1) || envelopes[0].EnvelopeID != "" {
				t.Fatalf("selection at %d: %+v", i+1, envelopes)
			}
			e := envelopes[0]
			if e.Seq < 9 && e.AgentVersion != "1" || e.Seq > 9 && e.AgentVersion != "2" {
				t.Fatalf("historical agent: %+v", e)
			}
			if e.Seq == 10 && e.RunID != "r" || (e.Seq == 12 || e.Seq == 14) && e.RunID != "new" {
				t.Fatalf("background original run: %+v", e)
			}
		}
		// 每个事件之间序列化，覆盖丢失内存但持久检查点仍在的恢复。
		encoded, err := json.Marshal(cp)
		if err != nil {
			t.Fatal(err)
		}
		cp = recovery.Checkpoint{}
		if err := json.Unmarshal(encoded, &cp); err != nil {
			t.Fatal(err)
		}
	}
	if len(cp.Questions) != 0 || len(cp.Background) != 0 {
		t.Fatalf("settled metadata retained: %+v", cp)
	}
}

func TestReplayRejectsGapAndWrongOwner(t *testing.T) {
	for _, raw := range []string{
		`{"session_id":"s","seq":"2","session_created":{"business_line":"bl","end_user":"u"}}`,
		`{"session_id":"other","seq":"1","session_created":{"business_line":"bl","end_user":"u"}}`,
		`{"session_id":"s","seq":"1","session_created":{"business_line":"bl","end_user":"other"}}`,
		`{"session_id":"s","seq":"1","run_completed":{"run_id":"r"}}`,
	} {
		cp := recovery.Checkpoint{SessionID: "s", EndUser: "u"}
		if _, err := recovery.Replay("bl", &cp, []json.RawMessage{json.RawMessage(raw)}); err == nil {
			t.Fatal("invalid event accepted")
		}
	}
}
