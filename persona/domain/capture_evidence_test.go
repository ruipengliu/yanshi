package domain

import (
	"errors"
	"testing"
	"time"
)

type verifyLookupAccess struct {
	CaptureAccess
	counts  map[Ref]int
	failRef Ref
	failure error
}

func (a *verifyLookupAccess) Lookup(date string, ref Ref) (CaptureSegment, bool, error) {
	a.counts[ref]++
	if ref == a.failRef {
		return CaptureSegment{}, false, a.failure
	}
	return a.CaptureAccess.Lookup(date, ref)
}

func TestVerifyReadsAllEvidenceAndPropagatesFailure(t *testing.T) {
	g := CaptureSegment{Speaker: "wearer", Text: "甲🙂明天交稿"}
	b := captureTestBook(t, []CaptureSegment{g, g.Clone()})
	failure := errors.New("lookup failed")
	a := &verifyLookupAccess{CaptureAccess: b.CaptureAccess(), counts: map[Ref]int{}, failRef: Ref{Seq: 1, Index: 1}, failure: failure}
	b.SetCaptureAccess(a)
	cites := map[string]Citation{"#1.0": {Speaker: "wearer"}, "#1.1": {Speaker: "wearer"}}
	c := NewCall(b, "s", nil, cites, captureTestNow)
	if got := c.verify("mine", []string{"#1.0", "#1.0", "#1.1"}); got != "" || !errors.Is(c.Err(), failure) {
		t.Fatalf("failure became verification: %q %v", got, c.Err())
	}
	if a.counts[Ref{Seq: 1}] != 1 || a.counts[Ref{Seq: 1, Index: 1}] != 1 {
		t.Fatal("repeated reference not cached or qualified first reference stopped lookup")
	}
}

func TestVerifyPlatformAndLowSupport(t *testing.T) {
	for _, kinds := range [][]string{nil, {"unknown"}, {"person", "date"}, {"none"}} {
		for _, reverse := range []bool{false, true} {
			g := CaptureSegment{Speaker: "wearer", Text: "甲🙂明天交稿"}
			context := g.Clone()
			context.LowSupportSpans = []LowSupportSpan{{Start: 1, End: 4, CriticalKinds: kinds}}
			b := captureTestBook(t, []CaptureSegment{g, context})
			refs := []string{"#1.0", "#1.1"}
			if reverse {
				refs[0], refs[1] = refs[1], refs[0]
			}
			c := NewCall(b, "s", nil, map[string]Citation{"#1.0": {Speaker: "wearer"}, "#1.1": {Speaker: "wearer"}}, captureTestNow)
			want := "引用的原话里有识别不可靠的内容"
			if len(kinds) == 1 && kinds[0] == "none" {
				want = ""
			}
			if got := c.verify("mine", refs); got != want || c.Err() != nil {
				t.Fatalf("kinds=%v reverse=%t: %q %v", kinds, reverse, got, c.Err())
			}
		}
	}
	b := captureTestBook(t, []CaptureSegment{{Speaker: "wearer", Text: "合格原文"}})
	for _, cites := range []map[string]Citation{nil, {"#1.0": {Speaker: "wearer", Missing: true}}} {
		c := NewCall(b, "s", nil, cites, captureTestNow)
		if got := c.verify("mine", []string{"#1.0"}); got != "引用的原话不存在" || c.Err() != nil {
			t.Fatalf("local lookup bypassed platform: %q %v", got, c.Err())
		}
	}
	c := NewCall(b, "s", nil, map[string]Citation{"#1.0": {Speaker: "other"}}, captureTestNow)
	if got := c.verify("theirs", []string{"#1.0"}); got != "引用的原话里没有对方说的话" {
		t.Fatalf("platform speaker replaced original: %q", got)
	}
}

func TestCaptureEvidenceCacheExpires(t *testing.T) {
	b := captureTestBook(t, []CaptureSegment{{Speaker: "wearer", Text: "原文"}})
	c := NewCall(b, "s", nil, nil, captureTestNow)
	if _, found := c.captureRef("2026-10-27", "#1.0"); !found || c.Err() != nil {
		t.Fatal("initial lookup failed", c.Err())
	}
	expires := b.BusinessDay("2026-10-27").ExpiresAt
	b.SetCaptureClock(func() time.Time { return expires })
	if _, found := c.captureRef("2026-10-27", "#1.0"); found || !errors.Is(c.Err(), ErrCapturesExpired) {
		t.Fatal("cache extended capture deadline", c.Err())
	}
}
