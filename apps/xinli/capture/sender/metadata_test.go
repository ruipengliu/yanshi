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

func TestMetadataRejectedBeforePersist(t *testing.T) {
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
		"unknown":     func(g *capture.Segment) { g.Speaker = "unknown" },
		"grade":       func(g *capture.Segment) { g.SpeakerGrade = "evidence" },
		"disposition": func(g *capture.Segment) { g.Disposition = "accept" },
		"reason":      func(g *capture.Segment) { g.DispositionReason = "playback" },
		"background":  func(g *capture.Segment) { g.Speaker = "other-1"; g.Background = true },
		"low_support": func(g *capture.Segment) {
			g.LowSupportSpans = []domain.LowSupportSpan{{Start: 0, End: 1, CriticalKinds: []string{"person"}}}
		},
		"empty_low_support": func(g *capture.Segment) { g.LowSupportSpans = []domain.LowSupportSpan{} },
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

func TestRestoredUnknownRejected(t *testing.T) {
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
	raw = bytes.ReplaceAll(raw, []byte(`"speaker":"wearer"`), []byte(`"speaker":"unknown"`))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := sender.Open(path, id)
	if restored != nil {
		restored.Close()
	}
	if !errors.Is(err, domain.ErrCaptureInvalid) {
		t.Fatal("restored unknown accepted", err)
	}
}
