package domain

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestEmptyWeekRetryAndRebinding(t *testing.T) {
	b := NewBook()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	b.BindWeek("2026-10-21", "week")
	for _, args := range []string{`{}`, `{"habits":[],"suggestions":[]}`, `{"suggestions":null}`} {
		c := NewCall(b, "week", []byte(args), nil, now)
		if text := c.FileWeek(); c.Err() != nil || text != "已收到本周的觉察（0 个习惯）" {
			t.Fatal(text, c.Err())
		}
	}
	first := b.PendingDomainChanges()
	if len(first) != 1 || first[0].Date != "2026-10-21" || first[0].Version != 1 {
		t.Fatal("empty retry was not stable", first)
	}
	b.PruneDomainChanges(now.Add(100 * DomainChangeRetention))
	if !reflect.DeepEqual(first, b.PendingDomainChanges()) {
		t.Fatal("unacknowledged change expired")
	}
	// 绑定决定周起点，提交时钟与 Session 换绑都不改变事实 ID。
	b.BindWeek("2026-10-21", "replacement")
	c := NewCall(b, "replacement", []byte(`{}`), nil, now.AddDate(0, 0, 2))
	c.FileWeek()
	if c.Err() != nil {
		t.Fatal(c.Err())
	}
	changes := b.PendingDomainChanges()
	if len(changes) != 2 || changes[1].ID != first[0].ID || changes[1].Version != 2 || len(b.State().Weeks) != 1 || b.State().Weeks[0].Session != "replacement" {
		t.Fatal("rebinding duplicated fact", changes, b.State().Weeks)
	}
}

func TestFactVersionExhaustionDoesNotOverwriteContent(t *testing.T) {
	b := NewBook()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	c := NewCall(b, "day", []byte(`{"day":{"headline":"原总览"}}`), nil, now)
	c.FileDay()
	id := b.PendingDomainChanges()[0].ID
	f := b.acceptedFacts[id]
	f.Version = ^uint64(0)
	b.acceptedFacts[id] = f
	before, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	c = NewCall(b, "day", []byte(`{"day":{"headline":"新总览"}}`), nil, now)
	c.FileDay()
	if c.Err() == nil || c.Filed != nil {
		t.Fatal("exhausted version accepted")
	}
	after, err := json.Marshal(b)
	if err != nil || string(before) != string(after) {
		t.Fatal("overflow mutated fact", err)
	}
}

func TestUnboundFactIdentityAndVersion(t *testing.T) {
	for _, kind := range []string{FactDay, FactWeek} {
		t.Run(kind, func(t *testing.T) {
			b := newCaptureTestBook()
			now := captureTestNow
			date := now.Format(time.DateOnly)
			if kind == FactDay {
				if err := b.BindDay(date, "bound"); err != nil {
					t.Fatal(err)
				}
			} else {
				date = now.AddDate(0, 0, -weekDays+1).Format(time.DateOnly)
				b.BindWeek(date, "bound")
			}
			file := func(session, args string, at time.Time) {
				t.Helper()
				c := NewCall(b, session, []byte(args), nil, at)
				if kind == FactDay {
					c.FileDay()
				} else {
					c.FileWeek()
				}
				if c.Err() != nil {
					t.Fatal(c.Err())
				}
			}
			args := `{"day":{"headline":"总览"},"suggestions":["沟通建议"]}`
			sessions := []string{"bound", "legacy1", "legacy2", "2026-10-28/session/legacy1"}
			for _, session := range sessions {
				file(session, args, now)
			}
			first := b.PendingDomainChanges()
			if len(first) != len(sessions) {
				t.Fatal("sessions shared an accepted fact", first)
			}
			seen := map[string]bool{}
			for _, change := range first {
				if seen[change.ID] || change.Version != 1 || change.Date != date {
					t.Fatal("fact identity collision", first)
				}
				seen[change.ID] = true
			}
			file("legacy1", args, now.Add(24*time.Hour))
			if !reflect.DeepEqual(first, b.PendingDomainChanges()) {
				t.Fatal("unbound retry changed identity or version")
			}
			file("legacy1", `{"day":{"headline":"修订总览"},"suggestions":["修订建议"]}`, now)
			changes := b.PendingDomainChanges()
			if len(changes) != len(first)+1 || changes[len(first)].ID != first[1].ID || changes[len(first)].Version != 2 {
				t.Fatal("resubmission did not advance its own version", changes)
			}
			for i, change := range first {
				fact, ok := b.AcceptedFactByID(change.ID)
				version := uint64(1)
				if i == 1 {
					version = 2
				}
				if !ok || fact.ID != change.ID || fact.Version != version {
					t.Fatal("another session changed fact version", fact)
				}
			}
			if kind == FactDay {
				if _, err := b.EraseDay(date, now); err != nil {
					t.Fatal(err)
				}
				if len(b.days) != len(sessions)-1 {
					t.Fatal("business day erasure removed legacy summaries", b.days)
				}
				for _, change := range first[1:] {
					if _, ok := b.AcceptedFactByID(change.ID); !ok {
						t.Fatal("business day erasure invalidated legacy fact", change.ID)
					}
				}
			} else if len(b.weeks) != len(sessions) {
				t.Fatal("unbound week replaced another session", b.weeks)
			}
		})
	}
}
