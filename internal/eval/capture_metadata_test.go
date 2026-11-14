package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"yanshi/internal/eval/xinlifixture"
)

func loadMetadataCapture(t *testing.T, segment string, external bool) ([]*Case, error) {
	t.Helper()
	dir := t.TempDir()
	field := "captures: [" + segment + "]"
	if external {
		if err := os.Mkdir(filepath.Join(dir, "data"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "data", "captures.yaml"), []byte("- "+segment+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		field = "captures_file: data/captures.yaml"
	}
	writeCase(t, dir, "metadata", "name: metadata\nturns:\n  - input: 整理今天\n    "+field+"\n")
	return LoadSuite(dir)
}

func TestCaptureMetadataYAML(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(map[bool]string{false: "inline", true: "file"}[external], func(t *testing.T) {
			cases, err := loadMetadataCapture(t, `{speaker: wearer, speaker_grade: evidence, text: "先🙂，周五寄📦啊。", low_support_spans: [{text: 周五, critical_kinds: [date]}, {text: "📦啊", critical_kinds: [none]}]}`, external)
			if err != nil {
				t.Fatal(err)
			}
			g := cases[0].Turns[0].Captures[0]
			want := []CaptureLowSupportSpan{{Start: 3, End: 5, CriticalKinds: []string{"date"}}, {Start: 6, End: 8, CriticalKinds: []string{"none"}}}
			if !reflect.DeepEqual(g.LowSupportSpans, want) || g.SpeakerGrade != "evidence" || g.Disposition != "" {
				t.Fatalf("loaded metadata = %+v", g)
			}
			cases, err = loadMetadataCapture(t, `{speaker: unknown, heard: unknown, text: "🙂周五", low_support_spans: [{start: 1, end: 3}]}`, external)
			if err != nil || !reflect.DeepEqual(cases[0].Turns[0].Captures[0].LowSupportSpans, []CaptureLowSupportSpan{{Start: 1, End: 3}}) {
				t.Fatalf("explicit rune span = %+v, %v", cases, err)
			}
			cases, err = loadMetadataCapture(t, `{speaker: wearer, text: 原话}`, external)
			if err != nil {
				t.Fatal(err)
			}
			if got := cases[0].Turns[0].Captures[0]; !reflect.DeepEqual(got, CaptureSegment{Speaker: "wearer", Text: "原话"}) {
				t.Fatalf("legacy defaults changed: %+v", got)
			}
		})
	}
}

func TestCaptureMetadataYAMLRejectsInvalid(t *testing.T) {
	for _, tc := range []struct{ name, segment, want string }{
		{"missing substring", `{speaker: wearer, text: 周五, low_support_spans: [{text: 周六}]}`, "no match"},
		{"repeated substring", `{speaker: wearer, text: 周五又周五, low_support_spans: [{text: 周五}]}`, "uniquely"},
		{"overlapping matches", `{speaker: wearer, text: 啊啊啊, low_support_spans: [{text: 啊啊}]}`, "uniquely"},
		{"mixed locations", `{speaker: wearer, text: 周五, low_support_spans: [{text: 周五, start: 0, end: 2}]}`, "mutually exclusive"},
		{"mixed null", `{speaker: wearer, text: 周五, low_support_spans: [{text: 周五, start: null}]}`, "mutually exclusive"},
		{"missing end", `{speaker: wearer, text: 周五, low_support_spans: [{start: 0}]}`, "both start and end"},
		{"empty substring", `{speaker: wearer, text: 周五, low_support_spans: [{text: ""}]}`, "nonempty"},
		{"negative", `{speaker: wearer, text: 周五, low_support_spans: [{start: -1, end: 1}]}`, "boundaries"},
		{"empty interval", `{speaker: wearer, text: 周五, low_support_spans: [{start: 1, end: 1}]}`, "boundaries"},
		{"out of range", `{speaker: wearer, text: "🙂周五", low_support_spans: [{start: 1, end: 4}]}`, "boundaries"},
		{"overlap", `{speaker: wearer, text: 周五寄出, low_support_spans: [{start: 0, end: 2}, {start: 1, end: 3}]}`, "order"},
		{"unordered", `{speaker: wearer, text: 周五寄出, low_support_spans: [{text: 寄出}, {text: 周五}]}`, "order"},
		{"unknown segment key", `{speaker: wearer, text: 周五, speaker_grdae: evidence}`, "unknown capture field"},
		{"unknown span key", `{speaker: wearer, text: 周五, low_support_spans: [{text: 周五, critical_kind: [date]}]}`, "unknown capture field"},
		{"asr conflict", `{speaker: wearer, text: 周五, asr: 周六, low_support_spans: [{text: 周五}]}`, "asr changes text"},
		{"asr empty spans", `{speaker: wearer, text: 周五, asr: 周六, low_support_spans: []}`, "asr changes text"},
		{"asr null spans", `{speaker: wearer, text: 周五, asr: 周六, low_support_spans: null}`, "asr changes text"},
		{"invalid kind", `{speaker: wearer, text: 周五, low_support_spans: [{text: 周五, critical_kinds: [dates]}]}`, "critical_kinds"},
		{"duplicate kind", `{speaker: wearer, text: 周五, low_support_spans: [{text: 周五, critical_kinds: [date, date]}]}`, "critical_kinds"},
		{"none mixed", `{speaker: wearer, text: 周五, low_support_spans: [{text: 周五, critical_kinds: [none, date]}]}`, "critical_kinds"},
		{"other grade", `{speaker: other, key: li, speaker_grade: evidence, text: 周五}`, "speaker_grade"},
		{"unknown key", `{speaker: unknown, key: li, text: 周五}`, "cannot have key"},
		{"unknown ref", `{speaker: unknown, speaker_ref: P-1, text: 周五}`, "speaker_ref"},
		{"invalid ref", `{speaker: other, key: li, speaker_ref: "P/1", text: 周五}`, "speaker_ref"},
		{"empty text", `{speaker: wearer, text: ""}`, "capture text"},
		{"invalid grade", `{speaker: wearer, speaker_grade: weak, text: 周五}`, "speaker_grade"},
		{"invalid disposition", `{speaker: other, text: 周五, disposition: hidden}`, "disposition"},
		{"missing reason", `{speaker: other, text: 周五, disposition: quarantine}`, "disposition_reason"},
		{"unexpected reason", `{speaker: other, text: 周五, disposition_reason: playback}`, "disposition_reason"},
		{"background wearer", `{speaker: wearer, text: 周五, background: true}`, "background"},
		{"noisy background wearer", `{speaker: other, key: li, text: 周五, background: true, heard: wearer}`, "noisy: invalid capture background"},
		{"invalid heard", `{speaker: wearer, text: 周五, heard: someone}`, "heard"},
	} {
		for _, external := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/inline", true: "/file"}[external], func(t *testing.T) {
				if _, err := loadMetadataCapture(t, tc.segment, external); err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("load error = %v; want %q", err, tc.want)
				}
			})
		}
	}
}

func TestCaptureHeardMetadataIsolation(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		source                   CaptureSegment
		speaker, grade, key, ref string
	}{
		{"wearer to unknown", CaptureSegment{Speaker: "wearer", SpeakerGrade: "timeline", Heard: "unknown"}, "unknown", "", "", ""},
		{"wearer to other", CaptureSegment{Speaker: "wearer", SpeakerGrade: "evidence", Heard: "other", HeardKey: "li"}, "other", "", "li", ""},
		{"other to wearer", CaptureSegment{Speaker: "other", Key: "li", SpeakerRef: "P-1", Heard: "wearer"}, "wearer", "", "", ""},
		{"other to unknown", CaptureSegment{Speaker: "other", Key: "li", SpeakerRef: "P-1", Heard: "unknown"}, "unknown", "", "", ""},
		{"same wearer", CaptureSegment{Speaker: "wearer", SpeakerGrade: "timeline", Heard: "wearer"}, "wearer", "timeline", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := tc.source
			g.Text = "周五"
			g.LowSupportSpans = []CaptureLowSupportSpan{{Start: 0, End: 2, CriticalKinds: []string{"date"}}}
			if err := validateCapture(g); err != nil {
				t.Fatal(err)
			}
			first, second := g.heard(), g.heard()
			if first.Speaker != tc.speaker || first.SpeakerGrade != tc.grade || first.Key != tc.key || first.SpeakerRef != tc.ref {
				t.Fatalf("heard = %+v", first)
			}
			first.LowSupportSpans[0].Start = 1
			first.LowSupportSpans[0].CriticalKinds[0] = "none"
			if !reflect.DeepEqual(second.LowSupportSpans, g.LowSupportSpans) || second.LowSupportSpans[0].Start != 0 || second.LowSupportSpans[0].CriticalKinds[0] != "date" {
				t.Fatal("heard metadata shared between trials")
			}
		})
	}
	if _, err := loadMetadataCapture(t, `{speaker: wearer, text: 周五, asr: 周五, low_support_spans: [{text: 周五}]}`, false); err != nil {
		t.Fatal("unchanged ASR should preserve offsets:", err)
	}
}

func TestCaptureFixtureLegacyJSON(t *testing.T) {
	raw, err := json.Marshal(xinlifixture.CaptureSegment{Speaker: "wearer", Text: "原话"})
	if err != nil || string(raw) != `{"speaker":"wearer","text":"原话"}` {
		t.Fatalf("legacy JSON = %s, %v", raw, err)
	}
}
