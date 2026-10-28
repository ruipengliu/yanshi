package capture_test

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/capture"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

func TestStructuredPageValidation(t *testing.T) {
	base := capture.StructuredPage{Kind: "page", Span: "57.0", Segments: []capture.PageSegment{{Ref: "57.0", Speaker: "wearer", Text: "原话"}}}
	for _, tc := range []struct {
		name   string
		change func(*capture.StructuredPage)
	}{
		{"kind", func(p *capture.StructuredPage) { p.Kind = "unknown" }},
		{"no segments", func(p *capture.StructuredPage) { p.Segments = nil }},
		{"limit", func(p *capture.StructuredPage) {
			for range capture.DefaultLimit {
				p.Segments = append(p.Segments, p.Segments[0])
			}
		}},
		{"long text", func(p *capture.StructuredPage) { p.Segments[0].Text = strings.Repeat("字", session.MaxCaptureText+1) }},
		{"ref", func(p *capture.StructuredPage) { p.Segments[0].Ref = "#57.0" }},
		{"negative ref", func(p *capture.StructuredPage) { p.Segments[0].Ref = "57.-1" }},
		{"overflow ref", func(p *capture.StructuredPage) { p.Segments[0].Ref = "18446744073709551616.0" }},
		{"duplicate ref", func(p *capture.StructuredPage) { p.Segments = append(p.Segments, p.Segments[0]) }},
		{"decreasing ref", func(p *capture.StructuredPage) {
			p.Segments = append(p.Segments, p.Segments[0])
			p.Segments[1].Ref = "56.0"
		}},
		{"span", func(p *capture.StructuredPage) { p.Span = "bad" }},
		{"span after first", func(p *capture.StructuredPage) { p.Span = "58.0" }},
		{"cont equal", func(p *capture.StructuredPage) { p.Cont = true }},
		{"speaker", func(p *capture.StructuredPage) { p.Segments[0].Speaker = "user" }},
		{"wearer key", func(p *capture.StructuredPage) { p.Segments[0].SpeakerKey = "other-1" }},
		{"other key missing", func(p *capture.StructuredPage) { p.Segments[0].Speaker = "other" }},
		{"other key invalid", func(p *capture.StructuredPage) {
			p.Segments[0].Speaker = "other"
			p.Segments[0].SpeakerKey = "bad\nkey"
		}},
		{"empty text", func(p *capture.StructuredPage) { p.Segments[0].Text = "" }},
		{"spoken at", func(p *capture.StructuredPage) { p.Segments[0].SpokenAt = "tomorrow" }},
		{"zone", func(p *capture.StructuredPage) { p.Segments[0].TimeZone = "Local" }},
		{"empty segments", func(p *capture.StructuredPage) { p.Kind = "empty" }},
		{"progress segments", func(p *capture.StructuredPage) { p.Kind = "in_progress" }},
		{"progress last at", func(p *capture.StructuredPage) { p.Kind = "in_progress"; p.Segments = nil; p.LastAt = "bad" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			p.Segments = append([]capture.PageSegment(nil), base.Segments...)
			tc.change(&p)
			raw, _ := json.Marshal(p)
			if _, err := capture.ParsePage(raw, capture.PageOptions{Limit: capture.DefaultLimit, MaxChars: capture.MaxChars}); err == nil {
				t.Fatal("invalid page accepted")
			}
		})
	}
	for _, raw := range []string{`null`, `{"kind":"empty","text":"untrusted"}`, `{"kind":"empty"} {}`, `{"kind":"page","segments":"wrong"}`} {
		if _, err := capture.ParsePage([]byte(raw), capture.PageOptions{Limit: 200, MaxChars: 6000}); err == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
	raw, _ := json.Marshal(base)
	p, err := capture.ParsePage(raw, capture.PageOptions{Limit: 200, MaxChars: 6000})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := capture.Render(p, 1); err == nil {
		t.Fatal("budget accepted")
	}
	base.Span, base.Segments[0].Ref = "0.0", "0.0"
	raw, _ = json.Marshal(base)
	p, err = capture.ParsePage(raw, capture.PageOptions{Limit: 200, MaxChars: 6000})
	if err != nil {
		t.Fatal(err)
	}
	_, next, err := capture.Render(p, 0)
	if err != nil || next.String() != "0.0" {
		t.Fatalf("zero ref cursor %s: %v", next, err)
	}
}

// max_chars 只决定页大小，不能以它代替单段长度和渲染后的 token 预算。
func TestStructuredPageCharacterLimits(t *testing.T) {
	p := capture.StructuredPage{Kind: "page", Span: "57.0", Done: true, Segments: []capture.PageSegment{{Ref: "57.0", Speaker: "wearer", Text: strings.Repeat("字", session.MaxCaptureText)}}}
	opts := capture.PageOptions{Limit: capture.MaxLimit, MaxChars: 1}
	raw, _ := json.Marshal(p)
	parsed, err := capture.ParsePage(raw, opts)
	if err != nil {
		t.Fatalf("first segment must be accepted: %v", err)
	}
	if _, _, err := capture.Render(parsed, 1); err == nil {
		t.Fatal("first segment escaped token budget")
	}
	if _, _, err := capture.Render(parsed, 8000); err != nil {
		t.Fatal(err)
	}
	p.Segments[0].Text += "字"
	raw, _ = json.Marshal(p)
	if _, err := capture.ParsePage(raw, opts); err == nil || err.Error() != "业务线返回的旁听页无效：片段文字" {
		t.Fatalf("segment text limit: %v", err)
	}

	p.Segments = []capture.PageSegment{{Ref: "57.0", Speaker: "wearer", Text: "甲"}, {Ref: "57.1", Speaker: "wearer", Text: "乙"}}
	raw, _ = json.Marshal(p)
	n := len([]rune("#57.0 " + model.CaptureText(&v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_WEARER, Text: "甲"})))
	opts.MaxChars = 2*n - 1
	if _, err := capture.ParsePage(raw, opts); err == nil || err.Error() != "业务线返回的旁听页无效：字符上限" {
		t.Fatalf("second segment must respect chars: %v", err)
	}
	opts.MaxChars++
	if _, err := capture.ParsePage(raw, opts); err != nil {
		t.Fatalf("exact character boundary: %v", err)
	}
}

func TestPageOptions(t *testing.T) {
	inv := capability.Invocation{Agent: &agentdef.Def{Context: agentdef.Context{MaxToolResult: 1234}}}
	o := capture.OptionsFor(inv)
	if o.Limit != capture.MaxLimit || o.MaxChars != capture.MaxChars || o.GapMS != 300000 || o.MaxToolResult != 1234 {
		t.Fatalf("options = %+v", o)
	}
}
