package session

import (
	"testing"

	v1 "yanshi/gen/yanshi/v1"
)

func attestedEvents() []*v1.Event {
	es := []*v1.Event{created(), requested("r1"), attempt("r1", 1),
		{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{RunId: "r1", Attempt: 1, ToolCalls: []*v1.ToolCall{{CallId: "page0", Capability: "mcp_xinli__captures"}}}}},
		started("r1", 1, "page0"),
		{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{RunId: "r1", Attempt: 1, CallId: "page0"}}},
	}
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
		{"external", func(es []*v1.Event) { es[4] = routed("r1", 1, "page0", "phone"); es[5].GetToolResult().Attempt = 0 }},
		{"node", func(es []*v1.Event) { es[4] = routed("r1", 1, "page0", "phone") }},
		{"non MCP", func(es []*v1.Event) { es[3].GetAssistantMessage().ToolCalls[0].Capability = "echo" }},
		{"malformed MCP", func(es []*v1.Event) { es[3].GetAssistantMessage().ToolCalls[0].Capability = "mcp_xinli" }},
		{"error", func(es []*v1.Event) { es[5].GetToolResult().IsError = true }},
		{"missing marker", func(es []*v1.Event) { es[5].GetToolResult().Attested = false }},
		{"bad ref", func(es []*v1.Event) { es[5].GetToolResult().Content[0].GetCapture().Ref = "#57.0" }},
		{"missing ref", func(es []*v1.Event) { es[5].GetToolResult().Content[0].GetCapture().Ref = "" }},
		// 内置的 captures 工具已退役（ADR-0050），它的结果不能再带旁听片段。
		{"builtin captures", func(es []*v1.Event) {
			es[3].GetAssistantMessage().ToolCalls[0].Capability = "captures"
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
	if ProjectionVersion <= 18 {
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
