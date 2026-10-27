package session

import (
	"testing"

	v1 "yanshi/gen/yanshi/v1"
)

func attestedEvents() []*v1.Event {
	es := ackEvents(nil, []*v1.CaptureAck{nil})
	r := es[len(es)-1].GetToolResult()
	r.Attested = true
	r.Content = []*v1.ContentBlock{{Kind: &v1.ContentBlock_Capture{Capture: &v1.CaptureSegment{Ref: "57.0", Speaker: v1.Speaker_SPEAKER_WEARER, Text: "原话"}}}}
	return es
}

func TestAttestedApply(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func([]*v1.Event)
	}{
		{"external", func(es []*v1.Event) { es[4] = routed("r1", 1, "ack0", "phone"); es[5].GetToolResult().Attempt = 0 }},
		{"node", func(es []*v1.Event) { es[4] = routed("r1", 1, "ack0", "phone") }},
		{"non MCP", func(es []*v1.Event) { es[3].GetAssistantMessage().ToolCalls[0].Capability = "echo" }},
		{"malformed MCP", func(es []*v1.Event) { es[3].GetAssistantMessage().ToolCalls[0].Capability = "mcp_xinli" }},
		{"error", func(es []*v1.Event) { es[5].GetToolResult().IsError = true }},
		{"read", func(es []*v1.Event) { es[5].GetToolResult().CaptureRead = ackPage("0", "57.0", "57.1") }},
		{"missing marker", func(es []*v1.Event) { es[5].GetToolResult().Attested = false }},
		{"bad ref", func(es []*v1.Event) { es[5].GetToolResult().Content[0].GetCapture().Ref = "#57.0" }},
		{"missing ref", func(es []*v1.Event) { es[5].GetToolResult().Content[0].GetCapture().Ref = "" }},
		{"builtin ref", func(es []*v1.Event) {
			es[3].GetAssistantMessage().ToolCalls[0].Capability = CapturesCapability
			es[5].GetToolResult().Attested = false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			es := attestedEvents()
			tc.change(es)
			if _, err := Reduce(build(es...)); err == nil {
				t.Fatal("invalid result accepted")
			}
		})
	}
	if _, err := Reduce(build(attestedEvents()...)); err != nil {
		t.Fatal(err)
	}
	if err := CheckCapture(&v1.CaptureAppended{CaptureId: "c", Stream: StreamTranscript, Segments: []*v1.CaptureSegment{{Speaker: v1.Speaker_SPEAKER_WEARER, Text: "原话", Ref: "57.0"}}}); err == nil {
		t.Fatal("appended ref accepted")
	}
	if ProjectionVersion <= 16 {
		t.Fatal("old snapshots remain valid")
	}
}

func TestAttestedPieces(t *testing.T) {
	for _, other := range []bool{false, true} {
		r := attestedEvents()[5].GetToolResult()
		if other {
			for _, text := range []string{"他人一句", "他人二句"} {
				r.Content = append(r.Content, &v1.ContentBlock{Kind: &v1.ContentBlock_Capture{Capture: &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_OTHER, SpeakerKey: "other-1", Ref: "58.0", Text: text}}})
			}
		}
		pieces := resultPieces(r, true, false)
		want := 1
		if other {
			want = 2
		}
		if len(pieces) != want || pieces[0].From != FromUser {
			t.Fatalf("pieces = %+v", pieces)
		}
		if other && (pieces[1].From != FromOther || pieces[1].Text != "他人一句\n他人二句" || pieces[1].Result != r) {
			t.Fatal("other speech not merged")
		}
	}
}
