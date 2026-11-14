package domain

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCaptureMetadataValidation(t *testing.T) {
	base := CaptureSegment{Speaker: "wearer", Text: "甲🙂\x00乙丙"}
	span := func(start, end int, kinds ...string) []LowSupportSpan {
		return []LowSupportSpan{{Start: start, End: end, CriticalKinds: kinds}}
	}
	for _, tc := range []struct {
		name   string
		change func(*CaptureSegment)
		valid  bool
	}{
		{"legacy", func(g *CaptureSegment) {}, true},
		{"unknown", func(g *CaptureSegment) { g.Speaker = "unknown" }, true},
		{"unknown_key", func(g *CaptureSegment) { g.Speaker = "unknown"; g.SpeakerKey = "x" }, false},
		{"unknown_ref", func(g *CaptureSegment) { g.Speaker = "unknown"; g.SpeakerRef = "P-1" }, false},
		{"other", func(g *CaptureSegment) { g.Speaker = "other"; g.SpeakerKey = "other-1"; g.SpeakerRef = "P-1" }, true},
		{"evidence", func(g *CaptureSegment) { g.SpeakerGrade = "evidence" }, true},
		{"timeline", func(g *CaptureSegment) { g.SpeakerGrade = "timeline" }, true},
		{"grade_invalid", func(g *CaptureSegment) { g.SpeakerGrade = "high" }, false},
		{"unknown_grade", func(g *CaptureSegment) { g.Speaker = "unknown"; g.SpeakerGrade = "evidence" }, false},
		{"other_grade", func(g *CaptureSegment) { g.Speaker = "other"; g.SpeakerKey = "x"; g.SpeakerGrade = "timeline" }, false},
		{"accept", func(g *CaptureSegment) { g.Disposition = "accept" }, true},
		{"playback", func(g *CaptureSegment) { g.Disposition = "quarantine"; g.DispositionReason = "playback" }, true},
		{"replay", func(g *CaptureSegment) { g.Disposition = "quarantine"; g.DispositionReason = "suspected_replay" }, true},
		{"disagreement", func(g *CaptureSegment) {
			g.Disposition = "quarantine"
			g.DispositionReason = "hallucination_disagreement"
		}, true},
		{"missing_reason", func(g *CaptureSegment) { g.Disposition = "quarantine" }, false},
		{"bad_reason", func(g *CaptureSegment) { g.Disposition = "quarantine"; g.DispositionReason = "bad" }, false},
		{"default_reason", func(g *CaptureSegment) { g.DispositionReason = "playback" }, false},
		{"accept_reason", func(g *CaptureSegment) { g.Disposition = "accept"; g.DispositionReason = "playback" }, false},
		{"bad_disposition", func(g *CaptureSegment) { g.Disposition = "reject" }, false},
		{"wearer_background", func(g *CaptureSegment) { g.Background = true }, false},
		{"other_background", func(g *CaptureSegment) { g.Speaker = "other"; g.SpeakerKey = "x"; g.Background = true }, true},
		{"unknown_background", func(g *CaptureSegment) { g.Speaker = "unknown"; g.Background = true }, true},
		{"runes", func(g *CaptureSegment) { g.LowSupportSpans = span(0, 5) }, true},
		{"adjacent", func(g *CaptureSegment) { g.LowSupportSpans = append(span(0, 1), span(1, 5, "none")...) }, true},
		{"gap", func(g *CaptureSegment) { g.LowSupportSpans = append(span(0, 1), span(3, 5, "unknown")...) }, true},
		{"negative", func(g *CaptureSegment) { g.LowSupportSpans = span(-1, 1) }, false},
		{"empty", func(g *CaptureSegment) { g.LowSupportSpans = span(1, 1) }, false},
		{"reversed", func(g *CaptureSegment) { g.LowSupportSpans = span(2, 1) }, false},
		{"byte_offset", func(g *CaptureSegment) { g.LowSupportSpans = span(0, 6) }, false},
		{"overlap", func(g *CaptureSegment) { g.LowSupportSpans = append(span(0, 3), span(2, 4)...) }, false},
		{"unsorted", func(g *CaptureSegment) { g.LowSupportSpans = append(span(3, 4), span(0, 1)...) }, false},
		{"duplicate", func(g *CaptureSegment) { g.LowSupportSpans = span(0, 1, "date", "date") }, false},
		{"none_mixed", func(g *CaptureSegment) { g.LowSupportSpans = span(0, 1, "person", "none") }, false},
		{"none_first", func(g *CaptureSegment) { g.LowSupportSpans = span(0, 1, "none", "unknown") }, false},
		{"kind_invalid", func(g *CaptureSegment) { g.LowSupportSpans = span(0, 1, "bad") }, false},
		{"kind_empty", func(g *CaptureSegment) { g.LowSupportSpans = span(0, 1, "") }, false},
		{"all_kinds", func(g *CaptureSegment) {
			g.LowSupportSpans = span(0, 1, "person", "date", "number", "negation", "modality", "correction", "unknown")
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := base.Clone()
			tc.change(&g)
			before := g.Clone()
			err := validateCaptureBatch("c", []CaptureSegment{g})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t, error=%v", tc.valid, err)
			}
			if !reflect.DeepEqual(g, before) {
				t.Fatal("validation mutated input")
			}
		})
	}
}

func TestCaptureMetadataDefaultsAndCopies(t *testing.T) {
	g := CaptureSegment{Speaker: "wearer", Text: "原文", LowSupportSpans: []LowSupportSpan{{Start: 0, End: 1}}}
	before := g.Clone()
	if g.EffectiveSpeakerGrade() != "evidence" || g.EffectiveDisposition() != "accept" || !reflect.DeepEqual(g.LowSupportSpans[0].EffectiveCriticalKinds(), []string{"unknown"}) {
		t.Fatal("wrong defaults")
	}
	if !reflect.DeepEqual(g, before) {
		t.Fatal("defaults written back")
	}
	g.Speaker = "other"
	if g.EffectiveSpeakerGrade() != "" {
		t.Fatal("other received evidence grade")
	}
	g.LowSupportSpans[0].CriticalKinds = []string{"person"}
	copy := g.Clone()
	copy.LowSupportSpans[0].CriticalKinds[0] = "date"
	copy.LowSupportSpans[0].End = 2
	if g.Equal(copy) || g.LowSupportSpans[0].End != 1 || g.LowSupportSpans[0].CriticalKinds[0] != "person" {
		t.Fatal("clone aliased")
	}
	kinds := g.LowSupportSpans[0].EffectiveCriticalKinds()
	kinds[0] = "date"
	if g.LowSupportSpans[0].CriticalKinds[0] != "person" {
		t.Fatal("effective kinds aliased")
	}
}

func TestCaptureLegacyJSONAndDigest(t *testing.T) {
	const legacy = `{"session_id":"s","seq":1,"capture_id":"c","business_day":"2026-10-27","time_zone":"UTC","generation":0,"segments":[{"speaker":"wearer","text":"原文","spoken_at":"2026-10-27T12:00:00Z","time_zone":"UTC"}]}`
	const digest = "ffb3a1d7d8eb603b709c63e0dc7d4b21986eb66b1a92d9990a53ec7716d5b07c"
	var in OrderedCapture
	if err := json.Unmarshal([]byte(legacy), &in); err != nil {
		t.Fatal(err)
	}
	in, err := NormalizeOrderedCapture(in)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(in)
	if err != nil || string(raw) != legacy {
		t.Fatalf("legacy JSON changed: %s, %v", raw, err)
	}
	b := newCaptureTestBook()
	if _, err := b.ReceiveCaptures("phone", in); err != nil {
		t.Fatal(err)
	}
	if b.captureStreams["s"].Batches[1].Digest != digest {
		t.Fatal("legacy digest changed")
	}
	for name, change := range map[string]func(*CaptureSegment){
		"speaker":     func(g *CaptureSegment) { g.Speaker = "unknown" },
		"grade":       func(g *CaptureSegment) { g.SpeakerGrade = "evidence" },
		"disposition": func(g *CaptureSegment) { g.Disposition = "accept" },
		"reason":      func(g *CaptureSegment) { g.Disposition = "quarantine"; g.DispositionReason = "playback" },
		"background":  func(g *CaptureSegment) { g.Speaker = "unknown"; g.Background = true },
		"low_support": func(g *CaptureSegment) {
			g.LowSupportSpans = []LowSupportSpan{{Start: 0, End: 1, CriticalKinds: []string{"person"}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := in
			changed.Segments = CloneCaptureSegments(in.Segments)
			change(&changed.Segments[0])
			if _, err := b.ReceiveCaptures("phone", changed); err != ErrCaptureConflict {
				t.Fatal("metadata not in digest", err)
			}
		})
	}
}

func TestNormalizeCaptureMetadataIsolation(t *testing.T) {
	in := OrderedCapture{SessionID: "s", Seq: 1, CaptureID: "c", TimeZone: "UTC", Segments: []CaptureSegment{{Speaker: "wearer", Text: "原文", SpokenAt: "2026-10-27T12:00:00Z", LowSupportSpans: []LowSupportSpan{{Start: 0, End: 1, CriticalKinds: []string{"person"}}}}}}
	before := in
	before.Segments = CloneCaptureSegments(in.Segments)
	out, err := NormalizeOrderedCapture(in)
	if err != nil {
		t.Fatal(err)
	}
	out.Segments[0].LowSupportSpans[0].End = 2
	out.Segments[0].LowSupportSpans[0].CriticalKinds[0] = "date"
	if !reflect.DeepEqual(in, before) {
		t.Fatal("normalization aliased input")
	}
	if out.Segments[0].SpeakerGrade != "" || out.Segments[0].Disposition != "" {
		t.Fatal("normalization wrote metadata defaults")
	}
}
