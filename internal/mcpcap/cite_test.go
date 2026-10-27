package mcpcap_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/mcpcap"
)

// 引用按发起调用的 Session 解析：佩戴者、他人、拦截的片段给出说话人；写错的编号、别的 Session 的编号、不是旁听的事件
// 都是 missing。带说话时间的片段附上 spoken_at 与 time_zone（业务线据此换算期限）。同一编号只解析一次，不是恰好一个编号的字符串不算引用。
func TestCitations(t *testing.T) {
	ctx := context.Background()
	log := memlog.New()
	appendTo := func(sid string, events ...*v1.Event) {
		for i, e := range events {
			e.SessionId, e.Seq = sid, uint64(i+1)
		}
		if _, err := log.Append(ctx, sid, 0, events...); err != nil {
			t.Fatal(err)
		}
	}
	created := func() *v1.Event {
		return &v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u"}}}
	}
	spoken := time.Date(2026, 10, 23, 2, 40, 0, 0, time.UTC)
	appendTo("s", created(), &v1.Event{Payload: &v1.Event_CaptureAppended{CaptureAppended: &v1.CaptureAppended{Stream: "transcript", CaptureId: "c", Segments: []*v1.CaptureSegment{
		{Speaker: v1.Speaker_SPEAKER_WEARER, Text: "报价单我明天发您", SpokenAt: timestamppb.New(spoken), TimeZone: "Asia/Shanghai"},
		{Speaker: v1.Speaker_SPEAKER_OTHER, SpeakerKey: "zhang", Text: "好"},
		{Speaker: v1.Speaker_SPEAKER_OTHER, SpeakerKey: "li", Redacted: true},
	}}}})
	appendTo("other", created(), created(), &v1.Event{Payload: &v1.Event_CaptureAppended{CaptureAppended: &v1.CaptureAppended{Stream: "transcript", CaptureId: "c",
		Segments: []*v1.CaptureSegment{{Speaker: v1.Speaker_SPEAKER_WEARER, Text: "别的 Session"}}}}})

	args := `{"commitments":[{"content":"发报价单 #2.0","refs":["#2.0","#2.1"]},{"refs":["#2.0","#2.2","#2.9","#1.0","#3.0"]}]}`
	got, err := mcpcap.Citations(ctx, log, "s", json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	want := []mcpcap.Citation{
		{Ref: "#2.0", Speaker: "wearer", SpokenAt: spoken, TimeZone: "Asia/Shanghai"},
		{Ref: "#2.1", Speaker: "other", SpeakerKey: "zhang"},
		{Ref: "#2.2", Speaker: "other", SpeakerKey: "li", Redacted: true},
		{Ref: "#2.9", Missing: true},
		{Ref: "#1.0", Missing: true},
		{Ref: "#3.0", Missing: true},
	}
	if g, w := mustJSON(t, got), mustJSON(t, want); g != w {
		t.Fatalf("citations = %s\nwant %s", g, w)
	}
	if got, err := mcpcap.Citations(ctx, log, "s", json.RawMessage(`not json`)); err != nil || len(got) != 0 {
		t.Fatalf("invalid arguments = %v, %v", got, err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAttestedCitationsLatestAndSnapshotPrefix(t *testing.T) {
	ctx := context.Background()
	log := citationNamespaceLog(t)
	for _, seq := range []uint64{0, 2, 3, 4} {
		got, err := mcpcap.AttestedCitations(ctx, log, "s", seq, json.RawMessage(`["#2.0","#2.1","#999.0"]`))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 || got[0].Missing != (seq == 2) || !got[1].Missing || !got[2].Missing {
			t.Fatalf("citations = %+v", got)
		}
		if seq == 3 && got[0].Speaker != "wearer" || (seq == 4 || seq == 0) && (got[0].Speaker != "other" || !got[0].Redacted || got[0].SpeakerKey != "other-1") {
			t.Fatal("wrong latest citation")
		}
	}
}

// 同一编号同时出现在内核 Capture 和业务线页中，用于验证登记边界。
func citationNamespaceLog(t *testing.T) *memlog.Log {
	t.Helper()
	ctx := context.Background()
	log := memlog.New()
	result := func(who v1.Speaker, redacted bool) *v1.Event {
		g := &v1.CaptureSegment{Ref: "2.0", Speaker: who, Redacted: redacted}
		if who == v1.Speaker_SPEAKER_OTHER {
			g.SpeakerKey = "other-1"
		}
		return &v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{Attested: true, Content: []*v1.ContentBlock{{Kind: &v1.ContentBlock_Capture{Capture: g}}}}}}
	}
	events := []*v1.Event{
		{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl"}}},
		{Payload: &v1.Event_CaptureAppended{CaptureAppended: &v1.CaptureAppended{Segments: []*v1.CaptureSegment{{Speaker: v1.Speaker_SPEAKER_WEARER}, {Speaker: v1.Speaker_SPEAKER_WEARER}}}}},
		result(v1.Speaker_SPEAKER_WEARER, false), result(v1.Speaker_SPEAKER_OTHER, true),
	}
	for i, e := range events {
		e.SessionId = "s"
		e.Seq = uint64(i + 1)
	}
	if _, err := log.Append(ctx, "s", 0, events...); err != nil {
		t.Fatal(err)
	}
	return log
}
