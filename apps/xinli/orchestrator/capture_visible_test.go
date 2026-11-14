package orchestrator

import (
	"context"
	"fmt"
	"testing"
	"time"

	"persona/domain"
)

func TestAutomaticReviewIgnoresHiddenReceipts(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		for _, allHidden := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/all_hidden=%v", backend, allHidden), func(t *testing.T) {
				ctx := context.Background()
				clock := &scheduleClock{}
				clock.set("2026-10-28T09:00:00Z")
				first := clock.now()
				books, jobs, _ := workStores(t, backend, clock.now)
				k := &scheduleKernel{books: books, now: clock.now, progress: true, file: true, workKernel: workKernel{terminal: "completed"}}
				o := New(ctx, books, k, Options{Now: clock.now, Tasks: jobs})
				defer finish(t, o)
				if !allHidden {
					appendScheduled(t, books, "2026-10-28", "visible", "UTC", first)
				}
				for i := 1; i <= 8; i++ {
					clock.at = first.Add(time.Duration(i) * time.Minute)
					if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
						_, err := b.AppendCaptures("2026-10-28", fmt.Sprint(i), []domain.CaptureSegment{{Speaker: "unknown", Text: "隐藏", Background: true, TimeZone: "UTC"}})
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if err := o.ScanSchedules(ctx); err != nil {
						t.Fatal(err)
					}
					want := 0
					if !allHidden && i >= 5 {
						want = 1
					}
					if len(k.inputs) != want {
						t.Fatalf("minute %d: hidden receipt changed scheduling: %d inputs, want %d", i, len(k.inputs), want)
					}
				}
				if err := books.View(ctx, "bl", "u", func(b *domain.Book) error {
					day := b.SchedulingDays()[0]
					if !day.Processed || !day.LastBatchAt.Equal(clock.now()) {
						t.Fatal("completion or receipt fact lost", day)
					}
					if allHidden {
						if b.CaptureProgress(day.Date) != "0" || !day.LastVisibleBatchAt.IsZero() {
							t.Fatal("all-hidden day fabricated progress", day)
						}
					} else if !day.LastVisibleBatchAt.Equal(first) || b.CaptureProgress(day.Date) != "1.0" {
						t.Fatal("hidden receipt changed visible metadata", day)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
