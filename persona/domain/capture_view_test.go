package domain

import (
	"encoding/json"
	"fmt"
	"iter"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func hiddenCapture(g CaptureSegment) CaptureSegment {
	g.Disposition, g.DispositionReason = "quarantine", "playback"
	return g
}

func TestProjectCapture(t *testing.T) {
	for _, tc := range []struct {
		name              string
		segment           CaptureSegment
		speaker, key, ref string
		visible           bool
	}{
		{"legacy", CaptureSegment{Speaker: "wearer"}, "wearer", "", "", true},
		{"evidence", CaptureSegment{Speaker: "wearer", SpeakerGrade: "evidence"}, "wearer", "", "", true},
		{"timeline", CaptureSegment{Speaker: "wearer", SpeakerGrade: "timeline"}, "other", "u-18446744073709551615-199", "", true},
		{"unknown", CaptureSegment{Speaker: "unknown"}, "other", "u-18446744073709551615-199", "", true},
		{"other", CaptureSegment{Speaker: "other", SpeakerKey: "o", SpeakerRef: "P-1"}, "other", "o", "P-1", true},
		{"quarantine", hiddenCapture(CaptureSegment{Speaker: "wearer"}), "", "", "", false},
		{"background", CaptureSegment{Speaker: "unknown", Background: true}, "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := tc.segment
			g.Text, g.SpokenAt, g.TimeZone = "原文", "2026-10-27T12:00:00Z", "UTC"
			ref := Ref{Seq: math.MaxUint64, Index: 199}
			p, visible := ProjectCapture(ref, g)
			if visible != tc.visible || p.Speaker != tc.speaker || p.SpeakerKey != tc.key || p.SpeakerRef != tc.ref {
				t.Fatalf("projection %+v, visible %v", p, visible)
			}
			if !visible {
				return
			}
			if p.Ref != ref.String() || p.Text != g.Text || p.SpokenAt != g.SpokenAt || p.TimeZone != g.TimeZone || p.SpeakerKey != "" && !captureSpeakerKey.MatchString(p.SpeakerKey) {
				t.Fatal("invalid projected fields", p)
			}
			again, _ := ProjectCapture(ref, g)
			if p != again {
				t.Fatal("unstable key")
			}
		})
	}
}

func TestVisibleCaptureSpansAndReadThrough(t *testing.T) {
	old := captureTestNow.Add(-time.Hour)
	g := captureTestSegment(old)
	b := captureTestBook(t, []CaptureSegment{
		hiddenCapture(captureTestSegment(captureTestNow)), g,
		hiddenCapture(captureTestSegment(old.Add(4 * time.Minute))),
		captureTestSegment(old.Add(8 * time.Minute)),
		hiddenCapture(captureTestSegment(captureTestNow)),
	})
	o := captureTestOptions()
	p := captureTestRead(t, b, `{}`, o)
	if p.Kind != "page" || !p.Ended || p.Span != "1.1" || len(p.Segments) != 1 {
		t.Fatal("hidden segment blocked or bridged a span", p)
	}
	d := b.captureDays["2026-10-27"]
	if d.ReadThrough.String() != "1.1" {
		t.Fatal("hidden segment advanced read tail", d.ReadThrough)
	}
	p = captureTestRead(t, b, `{"after":"1.1"}`, o)
	if p.Kind != "page" || !p.Done || p.Span != "1.3" || d.ReadThrough.String() != "1.3" {
		t.Fatal("hidden tail changed completion", p, d.ReadThrough)
	}
	// 隐藏尾段没有时间也不能抹掉可见尾段的进行中状态。
	b = captureTestBook(t, []CaptureSegment{captureTestSegment(captureTestNow), hiddenCapture(CaptureSegment{Speaker: "unknown", Text: "隐藏"})})
	p = captureTestRead(t, b, `{}`, o)
	if p.Kind != "in_progress" || b.captureDays["2026-10-27"].ReadThrough != (Ref{}) {
		t.Fatal("hidden timeless tail changed in-progress state", p)
	}
}

func TestVisibleCaptureProgressAndSource(t *testing.T) {
	old := captureTestNow.Add(-time.Hour)
	b := captureTestBook(t, []CaptureSegment{
		hiddenCapture(captureTestSegment(old)), captureTestSegment(old),
		hiddenCapture(captureTestSegment(old)), captureTestSegment(old.Add(time.Minute)),
		hiddenCapture(captureTestSegment(captureTestNow)),
	})
	d := b.captureDays["2026-10-27"]
	submit := func(from, to string) string {
		return NewCall(b, "s", json.RawMessage(fmt.Sprintf(`{"source":{"from":%q,"to":%q}}`, from, to)), nil, captureTestNow).FileChanges()
	}
	captureTestRead(t, b, `{"limit":1}`, captureTestOptions())
	if got := submit("#1.1", "#1.3"); strings.Contains(got, "没有被接受") || d.Progress != "0" {
		t.Fatal("unread visible tail advanced", got, d.Progress)
	}
	captureTestRead(t, b, `{"after":"1.1~1.1"}`, captureTestOptions())
	for _, pair := range [][2]string{{"#1.0", "#1.3"}, {"#1.1", "#1.4"}, {"#1.2", "#1.3"}} {
		if got := submit(pair[0], pair[1]); !strings.Contains(got, "没有被接受") || d.Progress != "0" {
			t.Fatal("hidden endpoint accepted", got, d.Progress)
		}
	}
	if got := submit("#1.1", "#1.3"); strings.Contains(got, "没有被接受") || d.Progress != "1.3" || d.ReadThrough.String() != "1.3" {
		t.Fatal("range across hidden ref rejected", got, d.Progress)
	}
	day, err := b.QueryDay("2026-10-27")
	if err != nil || !day.Processed || !b.SchedulingDays()[0].Processed {
		t.Fatal("physical tail prevented completion", day, err)
	}
}

func TestVisibleCaptureAllHidden(t *testing.T) {
	b := captureTestBook(t, []CaptureSegment{hiddenCapture(captureTestSegment(captureTestNow)), {Speaker: "unknown", Text: "背景", Background: true}})
	if p := captureTestRead(t, b, `{}`, captureTestOptions()); p.Kind != "empty" {
		t.Fatal(p)
	}
	d := b.captureDays["2026-10-27"]
	if !d.processed() || d.Progress != "0" || d.ReadThrough != (Ref{}) || !b.SchedulingDays()[0].LastVisibleBatchAt.IsZero() {
		t.Fatal("empty visible set fabricated progress", d)
	}
	if got := dayCapturesTest(t, b, "2026-10-27"); len(got) != 2 {
		t.Fatal("hidden captures erased from app view")
	}
}

func TestVisibleCaptureLookahead(t *testing.T) {
	for _, recent := range []bool{false, true} {
		old := captureTestNow.Add(-10 * time.Minute)
		want := "page"
		if recent {
			old, want = captureTestNow.Add(-2*time.Minute), "in_progress"
		}
		b := captureTestBook(t, []CaptureSegment{
			captureTestSegment(old), captureTestSegment(old.Add(time.Minute)),
			hiddenCapture(captureTestSegment(captureTestNow.Add(-4 * time.Minute))),
			hiddenCapture(CaptureSegment{Speaker: "unknown", Text: "隐藏无时间"}),
			hiddenCapture(captureTestSegment(captureTestNow)),
		})
		p := captureTestRead(t, b, `{"limit":1}`, captureTestOptions())
		if p.Kind != want {
			t.Fatalf("recent=%v: hidden lookahead changed state: %+v", recent, p)
		}
		if recent && b.captureDays["2026-10-27"].ReadThrough != (Ref{}) {
			t.Fatal("lookahead advanced read position")
		}
	}
}

func TestVisibleCaptureTimelessAndBudget(t *testing.T) {
	b := captureTestBook(t, []CaptureSegment{
		{Speaker: "unknown", Text: "无时间"}, hiddenCapture(CaptureSegment{Speaker: "wearer", Text: "隐藏"}),
		{Speaker: "wearer", SpeakerGrade: "timeline", Text: "下一页"},
	})
	p := captureTestRead(t, b, `{"limit":1}`, captureTestOptions())
	if captureTestAfter(p) != "1.0" || p.Segments[0].SpeakerKey != "u-1-0" {
		t.Fatal(p)
	}
	p = captureTestRead(t, b, `{"after":"1.0","limit":1}`, captureTestOptions())
	if p.Cont || !p.Done || p.Span != "1.2" || p.Segments[0].SpeakerKey != "u-1-2" {
		t.Fatal(p)
	}
	// 对照同编号的已投影片段，跨日日期头与预算边界必须在投影后计算。
	first := time.Date(2026, 10, 27, 23, 59, 0, 0, time.UTC)
	segments := []CaptureSegment{captureTestSegment(first), hiddenCapture(captureTestSegment(first)), captureTestSegment(first.Add(2 * time.Minute))}
	segments[0].Speaker, segments[2].SpeakerGrade = "unknown", "timeline"
	segments[0].Text, segments[2].Text = strings.Repeat("字", 50), strings.Repeat("字", 50)
	b = captureTestBook(t, segments)
	for _, chars := range []int{100, 250, 1000} {
		for _, budget := range []int{100, 250, 1000} {
			o := captureTestOptions()
			o.MaxChars, o.MaxToolResult = chars, budget
			expected := testCaptures{"2026-10-27": {1: make([]CaptureSegment, len(segments))}}
			for i, g := range segments {
				if p, ok := ProjectCapture(Ref{Seq: 1, Index: i}, g); ok {
					expected["2026-10-27"][1][i] = p.captureSegment().g
				}
			}
			plain := newCaptureTestBook()
			plain.SetCaptureAccess(expected)
			want, wantErr := capturePage(func(from Ref) iter.Seq2[captureSegment, error] {
				return plain.captureSegments("2026-10-27", from, Ref{})
			}, captureCursor{}, o, captureTestNow, true)
			got, gotErr := NewCall(b, "s", json.RawMessage(`{"all":true}`), nil, captureTestNow).Captures(o)
			if (wantErr == nil) != (gotErr == nil) || !reflect.DeepEqual(got, want) {
				t.Fatalf("chars=%d budget=%d: got %+v (%v), want %+v (%v)", chars, budget, got, gotErr, want, wantErr)
			}
		}
	}
}

func TestVisibleCaptureSchedulingLegacyAndErase(t *testing.T) {
	old := captureTestNow.Add(-time.Hour)
	b := captureTestBook(t, []CaptureSegment{captureTestSegment(old)})
	d := b.captureDays["2026-10-27"]
	// 旧快照没有新字段；隐藏追加必须在覆盖接收事实前固定回退值。
	d.VisibleTail, d.LastVisibleBatchAt = nil, time.Time{}
	original := d.LastBatchAt
	if !b.SchedulingDays()[0].LastVisibleBatchAt.Equal(original) {
		t.Fatal("legacy silence did not fall back to last receipt")
	}
	for i := 1; i <= 3; i++ {
		now := captureTestNow.Add(time.Duration(i) * time.Minute)
		b.SetCaptureClock(func() time.Time { return now })
		if _, err := b.AppendCaptures("2026-10-27", fmt.Sprint(i), []CaptureSegment{hiddenCapture(captureTestSegment(now))}); err != nil {
			t.Fatal(err)
		}
		s := b.SchedulingDays()[0]
		if !s.LastVisibleBatchAt.Equal(original) || !s.LastBatchAt.Equal(now) {
			t.Fatal("hidden receipt reset silence", s)
		}
	}
	clone, err := cloneCaptureTestBook(b)
	if err != nil || !clone.SchedulingDays()[0].LastVisibleBatchAt.Equal(original) {
		t.Fatal("visibility metadata lost in snapshot", err)
	}
	b = captureTestBook(t, []CaptureSegment{hiddenCapture(captureTestSegment(old)), captureTestSegment(old), hiddenCapture(captureTestSegment(old)), captureTestSegment(old.Add(20 * time.Minute)), captureTestSegment(old.Add(21 * time.Minute)), hiddenCapture(captureTestSegment(old))})
	d = b.captureDays["2026-10-27"]
	d.GapSet, d.GapMS, d.Progress = true, 300000, "1.4"
	if _, err := b.Erase("2026-10-27", "1.3", "1.3", captureTestNow); err != nil {
		t.Fatal(err)
	}
	if d.Progress != "1.1" || d.ReadThrough != (Ref{}) || d.VisibleTail.String() != "1.4" || d.Tail.String() != "1.5" {
		t.Fatal("rewind used hidden predecessor", d)
	}
}

func TestCaptureViewPreservesMetadata(t *testing.T) {
	segments := []CaptureSegment{
		{Speaker: "unknown", Text: "待核对", LowSupportSpans: []LowSupportSpan{{Start: 0, End: 1, CriticalKinds: []string{"person"}}}},
		hiddenCapture(CaptureSegment{Speaker: "wearer", SpeakerGrade: "timeline", Text: "隔离"}),
		{Speaker: "other", SpeakerKey: "o", SpeakerRef: "P-1", Text: "背景", Background: true},
	}
	b := captureTestBook(t, segments)
	all := dayCapturesTest(t, b, "2026-10-27")
	for i, got := range all {
		if got.Ref != fmt.Sprintf("1.%d", i) || !got.CaptureSegment.Equal(segments[i]) {
			t.Fatal("app view changed metadata", got)
		}
	}
	page, more, err := b.QueryCaptures("2026-10-27", "1.0", 1)
	if err != nil || !more || len(page) != 1 || page[0].Ref != "1.1" || !page[0].CaptureSegment.Equal(segments[1]) {
		t.Fatal("hidden query page lost", page, more, err)
	}
	all[0].LowSupportSpans[0].CriticalKinds[0] = "number"
	if dayCapturesTest(t, b, "2026-10-27")[0].LowSupportSpans[0].CriticalKinds[0] != "person" {
		t.Fatal("view aliased stored metadata")
	}
	legacy := CaptureSegment{Speaker: "wearer", Text: "旧数据"}
	view, _ := json.Marshal(CaptureView{Ref: "1.0", CaptureSegment: legacy})
	p, _ := ProjectCapture(Ref{Seq: 1}, legacy)
	trusted, _ := json.Marshal(p)
	if string(view) != string(trusted) {
		t.Fatalf("legacy query JSON changed: %s != %s", view, trusted)
	}
}
