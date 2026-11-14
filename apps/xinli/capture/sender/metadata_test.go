package sender_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"persona/domain"
	"xinli/capture"
	"xinli/capture/sender"
)

func TestInvalidMetadataRejectedBeforePersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	s, err := sender.Open(path, sender.Identity{BusinessLine: "bl", EndUser: "u", DeviceID: "phone", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*capture.Segment){
		"speaker":     func(g *capture.Segment) { g.Speaker = "invalid" },
		"grade":       func(g *capture.Segment) { g.SpeakerGrade = "invalid" },
		"disposition": func(g *capture.Segment) { g.Disposition = "invalid" },
		"reason":      func(g *capture.Segment) { g.DispositionReason = "playback" },
		"background":  func(g *capture.Segment) { g.Background = true },
		"low_support": func(g *capture.Segment) {
			g.LowSupportSpans = []domain.LowSupportSpan{{Start: 0, End: 3, CriticalKinds: []string{"person"}}}
		},
		"missing_reason": func(g *capture.Segment) { g.Disposition = "quarantine" },
	} {
		t.Run(name, func(t *testing.T) {
			good := capture.Segment{Speaker: "wearer", Text: "原文", At: "2026-10-27T12:00:00Z"}
			bad := good
			change(&bad)
			if _, err := s.Enqueue("2026-10-27", "UTC", 0, []capture.Segment{good, bad}); !errors.Is(err, domain.ErrCaptureInvalid) {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) || s.State().Next != 1 || len(s.State().Entries) != 0 {
				t.Fatal("invalid batch persisted")
			}
		})
	}
}

func TestRestoredInvalidMetadataRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	id := sender.Identity{BusinessLine: "bl", EndUser: "u", DeviceID: "phone", SessionID: "s"}
	s, err := sender.Open(path, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue("2026-10-27", "UTC", 0, []capture.Segment{{Speaker: "wearer", Text: "原文", At: "2026-10-27T12:00:00Z"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, fields := range []string{
		`"speaker":"invalid"`,
		`"speaker":"wearer","speaker_grade":"invalid"`,
		`"speaker":"wearer","disposition":"quarantine"`,
		`"speaker":"wearer","background":true`,
		`"speaker":"unknown","speaker_ref":"P-1"`,
		`"speaker":"wearer","low_support_spans":[{"start":0,"end":3}]`,
		`"speaker":"wearer","unexpected":0`,
		`"speaker":"wearer","low_support_spans":[{"start":0,"end":1,"unexpected":0}]`,
	} {
		changed := bytes.ReplaceAll(raw, []byte(`"speaker":"wearer"`), []byte(fields))
		if err := os.WriteFile(path, changed, 0600); err != nil {
			t.Fatal(err)
		}
		restored, err := sender.Open(path, id)
		if restored != nil {
			restored.Close()
		}
		if err == nil || !bytes.Contains([]byte(fields), []byte("unexpected")) && !errors.Is(err, domain.ErrCaptureInvalid) {
			t.Fatal("restored invalid metadata accepted", fields, err)
		}
	}
	// 旧恢复入口不重新校验时间；完整批次仍由入队和服务端接收校验。
	legacy := bytes.ReplaceAll(raw, []byte(`"at":"2026-10-27T12:00:00Z"`), []byte(`"at":""`))
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := sender.Open(path, id)
	if err != nil {
		t.Fatal("legacy restore acceptance changed", err)
	}
	defer restored.Close()
}
