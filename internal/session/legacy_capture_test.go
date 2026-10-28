package session

import (
	"strings"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
)

func legacyCaptureEvents() []*v1.Event {
	es := attestedEvents()
	es[3].GetAssistantMessage().ToolCalls[0].Capability = "captures"
	r := es[5].GetToolResult()
	r.Attested = false
	r.Content[0].GetCapture().Ref = ""
	return es
}

// 旧日志仍是事实源；重放不能把佩戴者原话降为外部内容，也不能把他人的话升级为用户依据。
func TestLegacyCaptureReplay(t *testing.T) {
	es := legacyCaptureEvents()
	r := es[5].GetToolResult()
	r.Content = append(textBlocks("【时段】平台说明"), r.Content...)
	for _, text := range []string{"他人一句", "他人二句", ""} {
		r.Content = append(r.Content, &v1.ContentBlock{Kind: &v1.ContentBlock_Capture{Capture: &v1.CaptureSegment{
			Speaker: v1.Speaker_SPEAKER_OTHER, SpeakerKey: "other-1", Text: text, Redacted: text == "",
		}}})
	}
	r.Content = append(r.Content, &v1.ContentBlock{Kind: &v1.ContentBlock_Capture{Capture: &v1.CaptureSegment{
		Speaker: v1.Speaker_SPEAKER_WEARER, Redacted: true,
	}}})
	r.CaptureRead = &v1.CaptureRead{Stream: "transcript", After: "57.0"}
	r.CaptureAck = &v1.CaptureAck{Stream: "transcript"}
	es = append(es, &v1.Event{Payload: &v1.Event_CaptureAppended{CaptureAppended: &v1.CaptureAppended{}}})
	st, err := Reduce(build(es...))
	if err != nil {
		t.Fatal(err)
	}
	pieces := st.Pieces()
	if len(pieces) != 2 {
		t.Fatalf("pieces = %+v", pieces)
	}
	if pieces[0].From != FromUser || pieces[0].Text != "原话" || pieces[0].Result != nil {
		t.Fatalf("wearer provenance = %+v", pieces[0])
	}
	if pieces[1].From != FromOther || pieces[1].Text != "他人一句\n他人二句" || pieces[1].Result != r {
		t.Fatalf("other provenance = %+v", pieces[1])
	}
}

func TestLegacyCaptureReplayValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func([]*v1.Event)
	}{
		{"other builtin", func(es []*v1.Event) { es[3].GetAssistantMessage().ToolCalls[0].Capability = "echo" }},
		{"MCP", func(es []*v1.Event) { es[3].GetAssistantMessage().ToolCalls[0].Capability = "mcp_xinli__captures" }},
		{"similar name", func(es []*v1.Event) { es[3].GetAssistantMessage().ToolCalls[0].Capability = "captures_extra" }},
		{"external result", func(es []*v1.Event) {
			es[4] = routed("r1", 1, "page0", "phone")
			es[5].GetToolResult().Attempt = 0
		}},
		{"attested marker", func(es []*v1.Event) { es[5].GetToolResult().Attested = true }},
		{"ref", func(es []*v1.Event) { es[5].GetToolResult().Content[0].GetCapture().Ref = "57.0" }},
		{"speaker", func(es []*v1.Event) { es[5].GetToolResult().Content[0].GetCapture().Speaker = v1.Speaker(0) }},
		{"wearer key", func(es []*v1.Event) { es[5].GetToolResult().Content[0].GetCapture().SpeakerKey = "other-1" }},
		{"missing other key", func(es []*v1.Event) { es[5].GetToolResult().Content[0].GetCapture().Speaker = v1.Speaker_SPEAKER_OTHER }},
		{"invalid other key", func(es []*v1.Event) {
			g := es[5].GetToolResult().Content[0].GetCapture()
			g.Speaker, g.SpeakerKey = v1.Speaker_SPEAKER_OTHER, "含文字"
		}},
		{"empty text", func(es []*v1.Event) { es[5].GetToolResult().Content[0].GetCapture().Text = "" }},
		{"redacted text", func(es []*v1.Event) { es[5].GetToolResult().Content[0].GetCapture().Redacted = true }},
		{"long text", func(es []*v1.Event) {
			es[5].GetToolResult().Content[0].GetCapture().Text = strings.Repeat("字", MaxCaptureText+1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			es := legacyCaptureEvents()
			tc.change(es)
			if _, err := Reduce(build(es...)); err == nil {
				t.Fatal("invalid legacy capture result accepted")
			}
		})
	}
	// 上限按字符数计算，不能因中文的 UTF-8 字节数而拒绝合法旧日志。
	es := legacyCaptureEvents()
	es[5].GetToolResult().Content[0].GetCapture().Text = strings.Repeat("字", MaxCaptureText)
	if _, err := Reduce(build(es...)); err != nil {
		t.Fatal(err)
	}
}
