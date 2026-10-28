package capture_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/capture"
	"yanshi/internal/eventlog/memlog"
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

// 保留现有 capture 测试原文，用同一批日志另验证结构化路径的每一个内容块（只有平台 ref 字段不同）。
func TestStructuredPageRenderingMatchesBuiltin(t *testing.T) {
	ctx := context.Background()
	log := memlog.New()
	at := time.Date(2026, 10, 27, 15, 59, 0, 0, time.UTC)
	gs := []*v1.CaptureSegment{
		{Speaker: v1.Speaker_SPEAKER_WEARER, Text: "原话", SpokenAt: timestamppb.New(at), TimeZone: "Asia/Shanghai"},
		{Speaker: v1.Speaker_SPEAKER_OTHER, SpeakerKey: "other-1", Text: "回答", SpokenAt: timestamppb.New(at.Add(2 * time.Minute)), TimeZone: "Asia/Shanghai"},
	}
	e := &v1.Event{SessionId: "s", Seq: 1, Payload: &v1.Event_CaptureAppended{CaptureAppended: &v1.CaptureAppended{Stream: "transcript", CaptureId: "c", Segments: gs}}}
	if _, err := log.Append(ctx, "s", 0, e); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		cursor capture.Cursor
		limit  int
		now    time.Time
		wire   capture.StructuredPage
	}{
		{"dates", capture.Cursor{}, 200, time.Time{}, capture.StructuredPage{Kind: "page", Span: "1.0", Done: true}},
		{"first page", capture.Cursor{}, 1, time.Time{}, capture.StructuredPage{Kind: "page", Span: "1.0"}},
		{"continuation", capture.Cursor{Last: capture.Ref{Seq: 1, Index: 0}, Span: capture.Ref{Seq: 1, Index: 0}}, 200, time.Time{}, capture.StructuredPage{Kind: "page", Span: "1.0", Cont: true, Done: true}},
		{"empty", capture.Cursor{Last: capture.Ref{Seq: 1, Index: 1}}, 200, time.Time{}, capture.StructuredPage{Kind: "empty"}},
		{"in progress", capture.Cursor{}, 200, at.Add(3 * time.Minute), capture.StructuredPage{Kind: "in_progress", Span: "1.0", LastAt: at.Add(2 * time.Minute).Format(time.RFC3339), TimeZone: "Asia/Shanghai"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, wn, err := capture.Page(ctx, log, "s", tc.cursor, capture.Options{Limit: tc.limit, MaxChars: capture.MaxChars, Gap: 5 * time.Minute, Now: tc.now})
			if err != nil {
				t.Fatal(err)
			}
			if tc.wire.Kind == "page" {
				start, end := 0, len(gs)
				if tc.name == "first page" {
					end = 1
				}
				if tc.name == "continuation" {
					start = 1
				}
				for i := start; i < end; i++ {
					g := gs[i]
					who := "wearer"
					if g.GetSpeaker() == v1.Speaker_SPEAKER_OTHER {
						who = "other"
					}
					tc.wire.Segments = append(tc.wire.Segments, capture.PageSegment{Ref: (capture.Ref{Seq: 1, Index: i}).String(), Speaker: who, SpeakerKey: g.GetSpeakerKey(), Text: g.GetText(), SpokenAt: g.GetSpokenAt().AsTime().Format(time.RFC3339), TimeZone: g.GetTimeZone()})
				}
			}
			raw, _ := json.Marshal(tc.wire)
			p, err := capture.ParsePage(raw, capture.PageOptions{Limit: 200, MaxChars: 6000})
			if err != nil {
				t.Fatal(err)
			}
			got, gn, err := capture.Render(p, 0)
			if err != nil {
				t.Fatal(err)
			}
			if model.Text(model.ExpandCaptures(got)) != model.Text(model.ExpandCaptures(want)) || gn.String() != wn.String() || len(got) != len(want) {
				t.Fatal("rendering changed")
			}
			for i, b := range got {
				copy := proto.Clone(b).(*v1.ContentBlock)
				if g := copy.GetCapture(); g != nil {
					g.Ref = ""
				}
				if !proto.Equal(copy, want[i]) {
					t.Fatalf("block %d differs", i)
				}
			}
		})
	}
}

func TestStructuredEndedPage(t *testing.T) {
	log := timedLog(t, []string{"2026-10-23T10:00 原话", "2026-10-23T11:00 下一时段"})
	want, wn, err := capture.Page(context.Background(), log, "s", capture.Cursor{}, capture.Options{Limit: 200, MaxChars: 6000, Gap: gap})
	if err != nil {
		t.Fatal(err)
	}
	p := capture.RenderPage{Kind: "page", Span: capture.Ref{Seq: 2}, Ended: true}
	for _, b := range want {
		if g := b.GetCapture(); g != nil {
			g = proto.Clone(g).(*v1.CaptureSegment)
			g.Ref = "2.0"
			p.Segments = append(p.Segments, g)
		}
	}
	got, gn, err := capture.Render(p, 0)
	if err != nil || wn.String() != gn.String() || model.Text(model.ExpandCaptures(want)) != model.Text(model.ExpandCaptures(got)) {
		t.Fatal("ended rendering differs")
	}
}

func TestPageOptionsFollowBuiltinParameters(t *testing.T) {
	inv := capability.Invocation{Agent: &agentdef.Def{Streams: []agentdef.Stream{{Name: capture.Stream, ExchangeGap: 9 * time.Minute}}, Context: agentdef.Context{MaxToolResult: 1234}}}
	o := capture.OptionsFor(inv)
	if o.Limit != capture.MaxLimit || o.MaxChars != capture.MaxChars || o.GapMS != (9*time.Minute).Milliseconds() || o.MaxToolResult != 1234 {
		t.Fatalf("options = %+v", o)
	}
	inv.State = &session.State{Captures: session.CaptureLog{Read: map[string]*v1.CaptureRead{capture.Stream: {ExchangeGapMs: 60000}}}}
	if capture.OptionsFor(inv).GapMS != 60000 {
		t.Fatal("committed gap changed")
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

// PageOptions.Limit 是可接受上限，改它不改变内置工具的默认 200 段或模型参数。
func TestPageOptionsDoNotChangeBuiltinLimit(t *testing.T) {
	log := appendLog(t, 2, 101)
	tool := capture.New(log, nil)
	for _, tc := range []struct {
		args string
		want int
	}{
		{`{"all":true}`, capture.DefaultLimit},
		{`{"all":true,"limit":201}`, 201},
		{`{"all":true,"limit":500}`, 202},
	} {
		blocks, err := tool.Invoke(context.Background(), capability.Invocation{SessionID: "s", Arguments: tc.args})
		if err != nil {
			t.Fatal(err)
		}
		if got := segments(blocks); got != tc.want {
			t.Fatalf("%s: %d segments, want %d", tc.args, got, tc.want)
		}
	}
}
