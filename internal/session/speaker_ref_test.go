package session

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
)

// attested 只证明来源，不能跳过片段本身的校验。
func TestCaptureSegmentValidation(t *testing.T) {
	for name, change := range map[string]func(*v1.CaptureSegment){
		"speaker":       func(g *v1.CaptureSegment) { g.Speaker = v1.Speaker(99) },
		"wearer key":    func(g *v1.CaptureSegment) { g.Speaker = v1.Speaker_SPEAKER_WEARER; g.SpeakerRef = "" },
		"missing key":   func(g *v1.CaptureSegment) { g.SpeakerKey = "" },
		"invalid key":   func(g *v1.CaptureSegment) { g.SpeakerKey = "bad key" },
		"long key":      func(g *v1.CaptureSegment) { g.SpeakerKey = strings.Repeat("x", 33) },
		"invalid ref":   func(g *v1.CaptureSegment) { g.SpeakerRef = "P\n0012" },
		"unicode ref":   func(g *v1.CaptureSegment) { g.SpeakerRef = "人物" },
		"long ref":      func(g *v1.CaptureSegment) { g.SpeakerRef = strings.Repeat("P", 65) },
		"wearer ref":    func(g *v1.CaptureSegment) { g.Speaker = v1.Speaker_SPEAKER_WEARER; g.SpeakerKey = "" },
		"empty text":    func(g *v1.CaptureSegment) { g.Text = "" },
		"redacted text": func(g *v1.CaptureSegment) { g.Redacted = true },
		"long text":     func(g *v1.CaptureSegment) { g.Text = strings.Repeat("字", MaxCaptureText+1) },
	} {
		t.Run(name, func(t *testing.T) {
			es := speakerRefEvents("P-0012")
			change(es[5].GetToolResult().Content[0].GetCapture())
			if _, err := Reduce(build(es...)); err == nil {
				t.Fatal("invalid segment accepted")
			}
		})
	}
}

func speakerRefEvents(ref string) []*v1.Event {
	es := attestedEvents()
	g := es[5].GetToolResult().Content[0].GetCapture()
	g.Speaker, g.SpeakerKey, g.SpeakerRef = v1.Speaker_SPEAKER_OTHER, "other-1", ref
	return es
}

func TestSpeakerRefReplayAndSnapshot(t *testing.T) {
	if ProjectionVersion <= 21 {
		t.Fatal("old snapshots remain valid")
	}
	if _, err := FromSnapshot(&v1.SessionSnapshot{Version: 21}); err == nil {
		t.Fatal("previous projection snapshot accepted")
	}
	for _, ref := range []string{"", "P-0012", strings.Repeat("P", 64)} {
		for _, redacted := range []bool{false, true} {
			es := speakerRefEvents(ref)
			if redacted {
				g := es[5].GetToolResult().Content[0].GetCapture()
				g.Text, g.Redacted = "", true
			}
			st, err := Reduce(build(es...))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := proto.Marshal(st.Snapshot())
			if err != nil {
				t.Fatal(err)
			}
			var snap v1.SessionSnapshot
			if err := proto.Unmarshal(raw, &snap); err != nil {
				t.Fatal(err)
			}
			restored, err := FromSnapshot(&snap)
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(st.Snapshot(), restored.Snapshot()) {
				t.Fatal("snapshot differs from replay")
			}
			for _, state := range []*State{st, restored} {
				pieces := state.Pieces()
				if redacted {
					if len(pieces) != 0 {
						t.Fatal("redacted segment produced evidence")
					}
				} else if len(pieces) != 1 || pieces[0].From != FromOther {
					t.Fatal("recognized other lost external provenance")
				}
			}
		}
	}
}
