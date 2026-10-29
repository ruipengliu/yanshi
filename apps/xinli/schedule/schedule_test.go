package schedule

import (
	"testing"
	"time"
)

func TestCalendarTimes(t *testing.T) {
	for _, zone := range []string{"Asia/Shanghai", "America/New_York"} {
		z, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		for _, date := range []string{"2026-10-28", "2026-03-07", "2026-10-31"} {
			d, _ := time.ParseInLocation(time.DateOnly, date, z)
			last := time.Date(d.Year(), d.Month(), d.Day(), 15, 0, 0, 0, z)
			day, final, err := DayTimes(date, z, last, 12*time.Minute)
			if err != nil || !day.Equal(last.Add(5*time.Minute)) || final.In(z).Hour() != 18 {
				t.Fatal(day, final, err)
			}
			next := d.AddDate(0, 0, 1)
			late := time.Date(next.Year(), next.Month(), next.Day(), 3, 30, 0, 0, z)
			_, final, err = DayTimes(date, z, late, 12*time.Minute)
			if err != nil || final.In(z).Format("2006-01-02 15:04") != next.Format(time.DateOnly)+" 04:12" {
				t.Fatal(final, err)
			}
		}
	}
}

func TestStableJitterAndWeekBoundary(t *testing.T) {
	z, _ := time.LoadLocation("Asia/Shanghai")
	j := Jitter("bl", "u", "2026-10-26")
	if j < 0 || j >= 30*time.Minute || j != Jitter("bl", "u", "2026-10-26") {
		t.Fatal(j)
	}
	if Jitter("a/b", "c", "d") == Jitter("a", "b/c", "d") {
		t.Fatal("ambiguous owner")
	}
	due := time.Date(2026, 11, 1, 23, 0, 0, 0, z).Add(j)
	from, got := Week(due, z, "bl", "u")
	if from != "2026-10-26" || !got.Equal(due) {
		t.Fatal(from, got)
	}
	from, _ = Week(due.Add(-time.Second), z, "bl", "u")
	if from != "2026-10-19" {
		t.Fatal(from)
	}
	from, got = Week(due.Add(24*time.Hour), z, "bl", "u")
	if from != "2026-10-26" || !got.Equal(due) {
		t.Fatal("restart changed week", from, got)
	}
}
