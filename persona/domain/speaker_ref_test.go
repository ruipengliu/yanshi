package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCaptureSpeakerRefValidation(t *testing.T) {
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
			b := newCaptureTestBook()
			g := CaptureSegment{Speaker: "other", SpeakerKey: "other-1", SpeakerRef: tc.ref, Text: "原话"}
			if tc.wearer {
				g.Speaker, g.SpeakerKey = "wearer", ""
			}
			_, err := b.AppendCaptures("2026-10-27", "a", []CaptureSegment{captureTestSegment(captureTestNow), g})
			if tc.invalid {
				if err == nil || len(b.captureDays) != 0 {
					t.Fatal("invalid batch accepted or partially stored")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := dayCapturesTest(t, b, "2026-10-27")[1].SpeakerRef; got != tc.ref {
				t.Fatalf("speaker ref = %q", got)
			}
		})
	}
}

func TestCaptureSpeakerRefPersistence(t *testing.T) {
	segs := []CaptureSegment{{Speaker: "other", SpeakerKey: "other-1", SpeakerRef: "P-0012", Text: "原话"}}
	b := captureTestBook(t, segs)
	segs[0].SpeakerRef = "P-mutated"
	clone, err := cloneCaptureTestBook(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, book := range []*Book{b, clone} {
		p := captureTestRead(t, book, `{"all":true}`, captureTestOptions())
		if p.Segments[0].SpeakerRef != "P-0012" {
			t.Fatal("page lost speaker ref")
		}
		p.Segments[0].SpeakerRef = "P-page"
		view := dayCapturesTest(t, book, "2026-10-27")
		if view[0].SpeakerRef != "P-0012" {
			t.Fatal("page aliases stored segment")
		}
		view[0].SpeakerRef = "P-view"
		if dayCapturesTest(t, book, "2026-10-27")[0].SpeakerRef != "P-0012" {
			t.Fatal("view aliases stored segment")
		}
	}
	clone.captures.(testCaptures)["2026-10-27"][1][0].SpeakerRef = "P-clone"
	if dayCapturesTest(t, b, "2026-10-27")[0].SpeakerRef != "P-0012" {
		t.Fatal("clone aliases stored segment")
	}
	if _, err := b.AppendCaptures("2026-10-27", "one", segs); err == nil {
		t.Fatal("changed ref accepted as duplicate")
	}
	for _, whole := range []bool{false, true} {
		book, err := cloneCaptureTestBook(b)
		if err != nil {
			t.Fatal(err)
		}
		if whole {
			_, err = book.EraseDay("2026-10-27", captureTestNow)
		} else {
			_, err = book.Erase("2026-10-27", "1.0", "1.0", captureTestNow)
		}
		if err != nil {
			t.Fatal(err)
		}
		if !book.captures.(testCaptures)["2026-10-27"][1][0].Equal(CaptureSegment{}) {
			t.Fatal("erasure left segment metadata")
		}
		raw, err := json.Marshal(book)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "P-0012") {
			t.Fatal("erasure left speaker ref in snapshot")
		}
	}
}

func TestCaptureSpeakerRefOldSnapshot(t *testing.T) {
	// 原文不再属于 Book 的 JSON；旧内存格式必须显式迁移，不能静默丢失。
	const old = `{"CaptureDays":{"2026-10-27":{"Session":"s","Batches":[{"ID":"one","Number":1,"Segments":[{"speaker":"other","speaker_key":"other-1","text":"旧原话"}]}],"Next":1,"Progress":"0"}}}`
	var b Book
	if err := json.Unmarshal([]byte(old), &b); err == nil {
		t.Fatal("legacy payload silently discarded")
	}
	// 无 speaker_ref 的片段和线上页仍省略该字段。
	var segment CaptureSegment
	if err := json.Unmarshal([]byte(`{"speaker":"other","speaker_key":"other-1","text":"旧原话"}`), &segment); err != nil {
		t.Fatal(err)
	}
	book := captureTestBook(t, []CaptureSegment{segment})
	p := captureTestRead(t, book, `{"all":true}`, captureTestOptions())
	if p.Segments[0].SpeakerRef != "" || p.Segments[0].Text != "旧原话" {
		t.Fatal("old segment changed")
	}
	for _, value := range []any{book, p, Citation{Ref: "#1.0", Speaker: "other", SpeakerKey: "other-1"}} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "speaker_ref") {
			t.Fatal("empty ref changed JSON")
		}
	}
}
