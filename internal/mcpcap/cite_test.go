package mcpcap_test

import (
	"context"
	"encoding/json"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/mcpcap"
)

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
		if seq == 3 && got[0].Speaker != "wearer" || (seq == 4 || seq == 0) && (got[0].Speaker != "other" || !got[0].Redacted || got[0].SpeakerKey != "other-1" || got[0].SpeakerRef != "P-0012") {
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
			g.SpeakerKey, g.SpeakerRef = "other-1", "P-0012"
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

func TestCitationWithoutSpeakerRefJSON(t *testing.T) {
	for _, c := range []mcpcap.Citation{
		{Ref: "#1.0", Speaker: "wearer"},
		{Ref: "#1.0", Speaker: "other", SpeakerKey: "other-1"},
	} {
		var wire map[string]any
		if err := json.Unmarshal([]byte(mustJSON(t, c)), &wire); err != nil {
			t.Fatal(err)
		}
		if _, ok := wire["speaker_ref"]; ok {
			t.Fatal("empty speaker ref was serialized")
		}
	}
}
