package capture

import (
	"encoding/json"
	"testing"

	"persona/domain"
)

func TestSegmentCopiesAndLegacyJSON(t *testing.T) {
	segments := []Segment{{Speaker: "wearer", Text: "原文", At: "2026-10-27T12:00:00Z", LowSupportSpans: []domain.LowSupportSpan{{Start: 0, End: 1, CriticalKinds: []string{"person"}}}}}
	copy := CloneSegments(segments)
	copy[0].LowSupportSpans[0].CriticalKinds[0] = "date"
	copy[0].LowSupportSpans[0].End = 2
	ordered := (Batch{Segments: segments}).Ordered()
	ordered.Segments[0].LowSupportSpans[0].CriticalKinds[0] = "number"
	if segments[0].LowSupportSpans[0].End != 1 || segments[0].LowSupportSpans[0].CriticalKinds[0] != "person" {
		t.Fatal("segment alias")
	}
	const legacy = `{"speaker":"wearer","text":"原文","at":"2026-10-27T12:00:00Z"}`
	var g Segment
	if err := json.Unmarshal([]byte(legacy), &g); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(g)
	if err != nil || string(raw) != legacy {
		t.Fatalf("legacy JSON changed: %s %v", raw, err)
	}
	for _, field := range []string{`"speaker_grade":""`, `"disposition":""`, `"disposition_reason":null`, `"background":false`, `"low_support_spans":[]`, `"low_support_spans":null`} {
		if err := json.Unmarshal([]byte(`{"speaker":"wearer","text":"原文",`+field+`}`), &g); err != nil {
			t.Fatalf("rejected %s: %v", field, err)
		}
	}
	for _, field := range []string{`"unexpected":0`, `"low_support_spans":[{"start":0,"end":1,"unexpected":0}]`} {
		if err := json.Unmarshal([]byte(`{"speaker":"wearer","text":"原文",`+field+`}`), &g); err == nil {
			t.Fatalf("accepted unknown field %s", field)
		}
	}
}
