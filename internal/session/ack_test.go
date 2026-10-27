package session

import (
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
)

func ackPage(start, first, after string) *v1.CaptureRead {
	return &v1.CaptureRead{Stream: StreamTranscript, Start: start, First: first, After: after, ExchangeGapMs: 300000, Ack: true}
}

func ackRange(from, through string) *v1.CaptureAck {
	return &v1.CaptureAck{Stream: StreamTranscript, From: from, Through: through}
}

func ackEvents(pages []*v1.CaptureRead, acks []*v1.CaptureAck) []*v1.Event {
	events := []*v1.Event{created(), requested("r1"), attempt("r1", 1)}
	for i, page := range pages {
		id := fmt.Sprintf("read%d", i)
		events = append(events, capturesCall("r1", 1, id), started("r1", 1, id),
			&v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{RunId: "r1", Attempt: 1, CallId: id, CaptureRead: page}}})
	}
	for i, ack := range acks {
		id := fmt.Sprintf("ack%d", i)
		events = append(events,
			&v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{RunId: "r1", Attempt: 1,
				ToolCalls: []*v1.ToolCall{{CallId: id, Capability: "mcp_xinli__file_changes"}}}}},
			started("r1", 1, id),
			&v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{RunId: "r1", Attempt: 1, CallId: id, CaptureAck: ack}}})
	}
	return events
}

func TestCaptureAckContinuousPages(t *testing.T) {
	pages := []*v1.CaptureRead{ackPage("0", "57.0", "57.1"), ackPage("57.1", "63.0", "63.2")}
	for _, tc := range []struct {
		name  string
		pages []*v1.CaptureRead
		acks  []*v1.CaptureAck
		want  string
	}{
		{"first only", pages, []*v1.CaptureAck{ackRange("57.0", "57.1")}, "57.1"},
		{"both", pages, []*v1.CaptureAck{ackRange("57.0", "63.2")}, "63.2"},
		{"separate receipts", pages, []*v1.CaptureAck{ackRange("63.0", "63.2"), ackRange("57.0", "57.1")}, "63.2"},
		{"second only", pages, []*v1.CaptureAck{ackRange("63.0", "63.2")}, ""},
		{"none", pages, nil, ""},
		{"continuation", []*v1.CaptureRead{ackPage("0", "57.0", "57.1~57.0"), ackPage("57.1~57.0", "57.2", "63.2")}, []*v1.CaptureAck{ackRange("57.0", "63.2")}, "63.2"},
		{"partial continuation", []*v1.CaptureRead{ackPage("0", "57.0", "57.1~57.0"), ackPage("57.1~57.0", "57.2", "63.2")}, []*v1.CaptureAck{ackRange("57.0", "57.1")}, "57.1~57.0"},
		{"reread farthest covered", []*v1.CaptureRead{ackPage("0", "57.0", "57.1"), ackPage("0", "57.0", "63.2"), ackPage("0", "57.0", "80.0")}, []*v1.CaptureAck{ackRange("57.0", "63.2")}, "63.2"},
		{"receipt beyond read", pages, []*v1.CaptureAck{ackRange("57.0", "999.0")}, "63.2"},
		{"cannot join partial receipts within page", pages[:1], []*v1.CaptureAck{ackRange("57.0", "57.0"), ackRange("57.1", "57.1")}, ""},
		{"missing read link", []*v1.CaptureRead{pages[0], ackPage("60.0", "63.0", "63.2")}, []*v1.CaptureAck{ackRange("57.0", "63.2")}, "57.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := build(append(ackEvents(tc.pages, tc.acks), completed("r1", 1))...)
			st, err := Reduce(events)
			if err != nil {
				t.Fatal(err)
			}
			if got := st.Captures.Read[StreamTranscript].GetAfter(); got != tc.want {
				t.Fatalf("committed %q, want %q", got, tc.want)
			}
			// 每个前缀都经真实字节往返再继续回放：页与回执不能只在内存里生效。
			for i := 1; i < len(events); i++ {
				prefix, err := Reduce(events[:i])
				if err != nil {
					t.Fatal(err)
				}
				raw, err := proto.Marshal(prefix.Snapshot())
				if err != nil {
					t.Fatal(err)
				}
				snap := &v1.SessionSnapshot{}
				if err := proto.Unmarshal(raw, snap); err != nil {
					t.Fatal(err)
				}
				restored, err := FromSnapshot(snap)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range events[i:] {
					if err := restored.Apply(e); err != nil {
						t.Fatal(err)
					}
				}
				if !Equal(st, restored) {
					t.Fatalf("snapshot at %d changed replay", i)
				}
			}
		})
	}
}

func TestCaptureAckRequiresWorkerMCPResult(t *testing.T) {
	for _, tc := range []struct {
		name, capability string
		n                uint32
		ack              *v1.CaptureAck
	}{
		{"external", "mcp_xinli__file_changes", 0, ackRange("57.0", "63.2")},
		{"other capability", "echo", 1, ackRange("57.0", "63.2")},
		{"malformed MCP name", "mcp_xinli", 1, ackRange("57.0", "63.2")},
		{"unknown stream", "mcp_xinli__file_changes", 1, &v1.CaptureAck{Stream: "photos", From: "57.0", Through: "63.2"}},
		{"bad ref", "mcp_xinli__file_changes", 1, ackRange("#57.0", "63.2")},
		{"cursor instead of ref", "mcp_xinli__file_changes", 1, ackRange("57.0", "63.2~57.0")},
		{"reversed", "mcp_xinli__file_changes", 1, ackRange("63.2", "57.0")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := ackEvents(nil, []*v1.CaptureAck{tc.ack})
			events[3].GetAssistantMessage().ToolCalls[0].Capability = tc.capability
			res := events[len(events)-1]
			res.GetToolResult().Attempt = tc.n
			if tc.n == 0 {
				events[len(events)-2] = routed("r1", 1, "ack0", "phone")
			}
			if _, err := Reduce(build(events...)); err == nil {
				t.Fatal("invalid ack accepted")
			}
		})
	}
}

func TestCaptureAckReadStructure(t *testing.T) {
	if ProjectionVersion <= 15 {
		t.Fatal("capture ack projection did not invalidate old snapshots")
	}
	for _, tc := range []struct{ start, first, after string }{
		{"new", "57.0", "57.1"}, {"0", "#57.0", "57.1"}, {"57.1", "57.0", "63.2"}, {"0", "63.2", "57.1"}, {"0", "57.0", "57.1~80.0"},
	} {
		if _, err := Reduce(build(ackEvents([]*v1.CaptureRead{ackPage(tc.start, tc.first, tc.after)}, nil)...)); err == nil {
			t.Fatalf("invalid page accepted: %+v", tc)
		}
	}
}

func TestCaptureAckResumesCommittedCursor(t *testing.T) {
	base := []*v1.Event{created(), requested("old"), attempt("old", 1), capturesCall("old", 1, "old-read"),
		started("old", 1, "old-read"), readResult("old", 1, "old-read", "50.2~50.0"), completed("old", 1)}
	pages := []*v1.CaptureRead{ackPage("50.2~50.0", "57.0", "63.2")}
	for _, ending := range []*v1.Event{completed("r1", 1), failed("r1", 1), interrupted("r1")} {
		for _, acks := range [][]*v1.CaptureAck{nil, {ackRange("57.0", "63.2")}} {
			events := append([]*v1.Event{}, base...)
			events = append(events, ackEvents(pages, acks)[1:]...)
			events = append(events, ending)
			st, err := Reduce(build(events...))
			if err != nil {
				t.Fatal(err)
			}
			want := "50.2~50.0"
			if ending.GetRunCompleted() != nil && len(acks) > 0 {
				want = "63.2"
			}
			if got := st.Captures.Read[StreamTranscript].GetAfter(); got != want {
				t.Fatalf("committed %s, want %s", got, want)
			}
		}
	}
}

func TestCaptureAckNotRequiredForUndeclaredStream(t *testing.T) {
	first, last := ackPage("0", "57.0", "57.1"), ackPage("57.1", "63.0", "63.2")
	first.Ack, last.Ack = false, false
	st, err := Reduce(build(append(ackEvents([]*v1.CaptureRead{first, last}, nil), completed("r1", 1))...))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Captures.Read[StreamTranscript].GetAfter(); got != "63.2" {
		t.Fatalf("committed = %s", got)
	}
}

func TestCaptureAckCloneDoesNotShareRunProgress(t *testing.T) {
	events := build(append(ackEvents([]*v1.CaptureRead{ackPage("0", "57.0", "57.1")}, []*v1.CaptureAck{ackRange("57.0", "57.1")}), completed("r1", 1))...)
	st, err := Reduce(events[:len(events)-1])
	if err != nil {
		t.Fatal(err)
	}
	cp := st.Clone()
	if err := cp.Apply(events[len(events)-1]); err != nil {
		t.Fatal(err)
	}
	if st.Captures.Read[StreamTranscript] != nil || cp.Captures.Read[StreamTranscript].GetAfter() != "57.1" {
		t.Fatal("clone changed committed position in original")
	}
}
