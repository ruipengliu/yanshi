package capture_test

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capture"
	"yanshi/internal/model"
)

func TestSpeakerRefPage(t *testing.T) {
	for _, tc := range []struct {
		name, ref       string
		wearer, invalid bool
	}{
		{name: "absent"},
		{name: "recognized", ref: "P-0012"},
		{name: "all characters", ref: "AZaz09_-"},
		{name: "maximum", ref: strings.Repeat("P", 64)},
		{name: "long", ref: strings.Repeat("P", 65), invalid: true},
		{name: "space", ref: "P 0012", invalid: true},
		{name: "newline", ref: "P\n0012", invalid: true},
		{name: "unicode", ref: "人物", invalid: true},
		{name: "punctuation", ref: "P.0012", invalid: true},
		{name: "wearer", ref: "P-0012", wearer: true, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := capture.PageSegment{Ref: "57.4", Speaker: "other", SpeakerKey: "other-1", SpeakerRef: tc.ref, Text: "原话\n下一句", SpokenAt: "2026-10-28T10:16:00+08:00", TimeZone: "Asia/Shanghai"}
			if tc.wearer {
				g.Speaker, g.SpeakerKey = "wearer", ""
			}
			raw, err := json.Marshal(capture.StructuredPage{Kind: "page", Span: "57.4", Done: true, Segments: []capture.PageSegment{g}})
			if err != nil {
				t.Fatal(err)
			}
			p, err := capture.ParsePage(raw, capture.PageOptions{Limit: 200, MaxChars: 6000})
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid speaker ref accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.Segments[0].GetSpeakerRef() != tc.ref {
				t.Fatal("speaker ref lost")
			}
			who := "他人 other-1"
			if tc.ref != "" {
				who += " @" + tc.ref
			}
			if got, want := model.CaptureText(p.Segments[0]), "10:16 ["+who+"] 原话 下一句\n"; got != want {
				t.Fatalf("render = %q, want %q", got, want)
			}
			p.Segments[0].Text, p.Segments[0].Redacted = "", true
			if got, want := model.CaptureText(p.Segments[0]), "10:16 ["+who+"]（这段话未通过内容安全检查，已拦截）\n"; got != want {
				t.Fatalf("redacted = %q, want %q", got, want)
			}
		})
	}
}

func TestSpeakerRefPageBudgets(t *testing.T) {
	p := capture.StructuredPage{Kind: "page", Span: "1.0", Done: true, Segments: []capture.PageSegment{
		{Ref: "1.0", Speaker: "other", SpeakerKey: "other-1", Text: "甲"},
		{Ref: "1.1", Speaker: "other", SpeakerKey: "other-2", Text: "乙"},
	}}
	g := &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_OTHER, SpeakerKey: "other-1", Text: "甲"}
	o := capture.PageOptions{Limit: 200, MaxChars: 2 * len([]rune("#1.0 "+model.CaptureText(g)))}
	raw, _ := json.Marshal(p)
	plain, err := capture.ParsePage(raw, o)
	if err != nil {
		t.Fatal(err)
	}
	blocks, _, err := capture.Render(plain, 0)
	if err != nil {
		t.Fatal(err)
	}
	plainBudget := model.EstimateTokens(blocks)
	p.Segments[1].SpeakerRef = "P-0012"
	raw, _ = json.Marshal(p)
	if _, err := capture.ParsePage(raw, o); err == nil {
		t.Fatal("speaker ref escaped character budget")
	}
	o.MaxChars += len(" @P-0012")
	withRef, err := capture.ParsePage(raw, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := capture.Render(withRef, plainBudget); err == nil {
		t.Fatal("speaker ref escaped token budget")
	}
	blocks, _, err = capture.Render(withRef, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := capture.Render(withRef, model.EstimateTokens(blocks)); err != nil {
		t.Fatal(err)
	}
}
