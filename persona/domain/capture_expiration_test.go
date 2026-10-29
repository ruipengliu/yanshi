package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCaptureExpiryCalendar(t *testing.T) {
	for _, tc := range []struct {
		date, zone, want string
		hours            int
	}{
		{"2026-03-01", "America/New_York", "2026-03-09T04:00:00Z", 191},
		{"2026-10-25", "America/New_York", "2026-11-02T05:00:00Z", 193},
		{"2026-03-01", "Asia/Shanghai", "2026-03-08T16:00:00Z", 192},
	} {
		t.Run(tc.date+tc.zone, func(t *testing.T) {
			got, err := CaptureExpiresAt(tc.date, tc.zone)
			if err != nil || got.Format(time.RFC3339) != tc.want || got.Location() != time.UTC {
				t.Fatal(got, err)
			}
			zone, _ := time.LoadLocation(tc.zone)
			start, _ := time.ParseInLocation(time.DateOnly, tc.date, zone)
			if got.Sub(start) != time.Duration(tc.hours)*time.Hour {
				t.Fatal("used elapsed hours instead of calendar days")
			}
		})
	}
}

func TestCaptureLifetimeAndRevision(t *testing.T) {
	b := newCaptureTestBook()
	const date = "2026-10-27"
	appendBatch := func(id, zone string) {
		t.Helper()
		if _, err := b.AppendCaptures(date, id, []CaptureSegment{{Speaker: "wearer", Text: "原文", TimeZone: zone}}); err != nil {
			t.Fatal(err)
		}
	}
	appendBatch("a", "America/New_York")
	first := b.BusinessDay(date)
	appendBatch("a", "America/New_York")
	if b.BusinessDay(date).CaptureRevision != 1 {
		t.Fatal("idempotent append changed revision")
	}
	appendBatch("b", "Asia/Shanghai")
	if err := b.BindDay(date, "old", captureTestNow.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Erase(date, "2.0", "2.0", captureTestNow); err != nil {
		t.Fatal(err)
	}
	if err := b.BindDay(date, "new"); err != nil {
		t.Fatal(err)
	}
	day := b.BusinessDay(date)
	if day.TimeZone != first.TimeZone || !day.ExpiresAt.Equal(first.ExpiresAt) || !day.DeleteAt.Equal(first.ExpiresAt) || day.CaptureRevision != 3 {
		t.Fatal(day)
	}
	b.captureDays[date].Progress = "1.0"
	b.captureDays[date].ReadThrough = Ref{Seq: 1}
	b.captureDays[date].GapSet = true
	b.Seed(SeedData{Projects: []Project{{ID: "P-1", Name: "保留"}}, Commitments: []SeedCommitment{{ID: "C-1", Kind: "mine", Content: "承诺"}}}, captureTestNow)
	NewCall(b, "new", json.RawMessage(`{"day":{"headline":"保留总览"}}`), nil, captureTestNow).FileDay()
	eraseCommit(b, Changeset{Session: "new", CreateExchanges: []Exchange{{ID: "x1.0", Session: "new", Date: date, From: "#1.0", To: "#1.0", Title: "保留交谈"}}})
	before := b.State()
	now := day.ExpiresAt.Add(-time.Nanosecond)
	b.SetCaptureClock(func() time.Time { return now })
	if _, err := b.DayCaptures(date); err != nil {
		t.Fatal(err)
	}
	now = day.ExpiresAt
	assertExpired := func(err error) {
		t.Helper()
		if !errors.Is(err, ErrCapturesExpired) {
			t.Fatal(err)
		}
	}
	_, err := b.DayCaptures(date)
	assertExpired(err)
	_, _, err = b.CaptureAccess().Lookup(date, Ref{Seq: 1})
	assertExpired(err)
	_, _, err = b.CaptureAccess().Adjacent(date, Ref{}, true)
	assertExpired(err)
	_, err = b.CaptureAccess().Batch(date, 1)
	assertExpired(err)
	_, err = b.AppendCaptures(date, "a", []CaptureSegment{{Speaker: "wearer", Text: "原文", TimeZone: "America/New_York"}})
	assertExpired(err)
	_, err = b.AppendCaptures("2020-01-01", "history", []CaptureSegment{{Speaker: "wearer", Text: "旧原文"}})
	assertExpired(err)
	if b.captureDays["2020-01-01"] != nil {
		t.Fatal("rejected history created a day")
	}
	for _, sid := range []string{"old", "new"} {
		c := NewCall(b, sid, json.RawMessage(`{}`), nil, now)
		if c.FileDay() != ErrCapturesExpired.Error() || c.FileChanges() != ErrCapturesExpired.Error() {
			t.Fatal("late write accepted")
		}
		_, err := c.Captures(captureTestOptions())
		assertExpired(err)
	}
	if err := b.ExpireCaptures(date, now); err != nil {
		t.Fatal(err)
	}
	if err := b.ExpireCaptures(date, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	d := b.captureDays[date]
	if !d.Expired || d.Deleted || d.CaptureRevision != 4 || d.Progress != "1.0" || d.ReadThrough != (Ref{}) || d.GapSet || d.GapMS != 0 || len(d.Sessions) != 2 || d.Next != 2 || len(d.BatchIDs) != 2 {
		t.Fatalf("wrong cleanup metadata: %+v", d)
	}
	if len(b.captures.(testCaptures)[date]) != 0 {
		t.Fatal("raw payload retained")
	}
	if !reflect.DeepEqual(before, b.State()) {
		t.Fatal("expiration cascaded ledger")
	}
	if err := b.Edit("C-1", "done", "", now); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(NewCall(b, "new", nil, nil, now).DayLedger(), "承诺") {
		t.Fatal("ledger query blocked")
	}
	if _, err := b.EraseDay(date, now); err != nil {
		t.Fatal("explicit day deletion after expiration", err)
	}
}

func TestCaptureScanExpiresWhilePaused(t *testing.T) {
	b := captureTestBook(t, []CaptureSegment{{Speaker: "wearer", Text: "一"}, {Speaker: "wearer", Text: "二"}})
	expires := b.BusinessDay(eraseDate).ExpiresAt
	now := expires.Add(-time.Second)
	b.SetCaptureClock(func() time.Time { return now })
	count := 0
	for _, err := range b.CaptureAccess().Scan(eraseDate, Ref{}, Ref{}) {
		if count == 0 {
			if err != nil {
				t.Fatal(err)
			}
			now = expires
		} else if !errors.Is(err, ErrCapturesExpired) {
			t.Fatal(err)
		}
		count++
	}
	if count != 2 {
		t.Fatal(count)
	}
}

func TestLegacyCaptureDeadline(t *testing.T) {
	var b Book
	if err := json.Unmarshal([]byte(`{"CaptureDays":{"2026-10-27":{"Next":2,"TimeZone":"America/New_York","Progress":"1.0","Session":"old"}}}`), &b); err != nil {
		t.Fatal(err)
	}
	first := b.BusinessDay(eraseDate)
	if first.ExpiresAt.IsZero() || first.TimeZone != "America/New_York" || !first.DeleteAt.Equal(first.ExpiresAt) {
		t.Fatal(first)
	}
	copy, err := b.Clone()
	if err != nil || copy.BusinessDay(eraseDate) != first {
		t.Fatal("legacy upgrade unstable", err)
	}
}
