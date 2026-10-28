package schedule_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"yanshi/internal/schedule"
)

func instant(t *testing.T, value string) time.Time {
	t.Helper()
	result, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func compile(t *testing.T, spec schedule.Spec, start string) *schedule.Plan {
	t.Helper()
	p, err := schedule.Compile(spec, instant(t, start))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCompile(t *testing.T) {
	const base = "FREQ=DAILY;BYHOUR=9;BYMINUTE=0"
	cases := []struct {
		name  string
		rule  string
		valid bool
	}{
		{"daily", base, true},
		{"case", "freq=weekly;byday=mo,fr;byhour=17;byminute=0", true},
		{"interval_min", base + ";INTERVAL=1", true},
		{"interval_max", base + ";INTERVAL=52", true},
		{"interval_zero", base + ";INTERVAL=0", false},
		{"interval_large", base + ";INTERVAL=53", false},
		{"interval_list", base + ";INTERVAL=1,2", false},
		{"interval_sign", base + ";INTERVAL=+1", false},
		{"count_min", base + ";COUNT=1", true},
		{"count_max", base + ";COUNT=1000", true},
		{"count_zero", base + ";COUNT=0", false},
		{"count_large", base + ";COUNT=1001", false},
		{"count_overflow", base + ";COUNT=999999999999999999999999999999999", false},
		{"until_utc", base + ";UNTIL=20270101T000000Z", true},
		{"until_local", base + ";UNTIL=20270101T000000", true},
		{"until_past", base + ";UNTIL=20260101T000000Z", false},
		{"until_equal", base + ";UNTIL=20261028T000000Z", false},
		{"until_offset", base + ";UNTIL=20270101T000000+0800", false},
		{"until_date", base + ";UNTIL=20270101", false},
		{"until_invalid_day", base + ";UNTIL=20270230T000000Z", false},
		{"until_fraction", base + ";UNTIL=20270101T000000.1Z", false},
		{"count_until", base + ";COUNT=3;UNTIL=20270101T000000Z", false},
		{"unknown", base + ";BYSECOND=0", false},
		{"duplicate", base + ";freq=DAILY", false},
		{"empty_value", base + ";COUNT=", false},
		{"trailing_separator", base + ";", false},
		{"multiple_equals", base + ";COUNT=1=2", false},
		{"no_freq", "BYHOUR=9;BYMINUTE=0", false},
		{"hourly", "FREQ=HOURLY;BYHOUR=9;BYMINUTE=0", false},
		{"no_hour", "FREQ=DAILY;BYMINUTE=0", false},
		{"no_minute", "FREQ=DAILY;BYHOUR=9", false},
		{"hour_bounds", "FREQ=DAILY;BYHOUR=0,23;BYMINUTE=59", true},
		{"hour_negative", "FREQ=DAILY;BYHOUR=-1;BYMINUTE=0", false},
		{"hour_large", "FREQ=DAILY;BYHOUR=24;BYMINUTE=0", false},
		{"hour_text", "FREQ=DAILY;BYHOUR=nine;BYMINUTE=0", false},
		{"minute_negative", "FREQ=DAILY;BYHOUR=9;BYMINUTE=-1", false},
		{"minute_large", "FREQ=DAILY;BYHOUR=9;BYMINUTE=60", false},
		{"hour_empty_item", "FREQ=DAILY;BYHOUR=9,;BYMINUTE=0", false},
		{"minimum_equal", "FREQ=DAILY;BYHOUR=9,10;BYMINUTE=30", true},
		{"minimum_short", "FREQ=DAILY;BYHOUR=9;BYMINUTE=0,59", false},
		{"minimum_wrap_equal", "FREQ=DAILY;BYHOUR=0,23;BYMINUTE=30", true},
		{"minimum_wrap_short", "FREQ=DAILY;BYHOUR=0,23;BYMINUTE=0,30", false},
		{"duplicate_list", "FREQ=DAILY;BYHOUR=9,9;BYMINUTE=0,0", true},
		{"all_weekdays", base + ";BYDAY=MO,TU,WE,TH,FR,SA,SU", true},
		{"bad_weekday", base + ";BYDAY=XX", false},
		{"empty_weekday", base + ";BYDAY=MO,", false},
		{"daily_ordinal", base + ";BYDAY=1MO", false},
		{"weekly_ordinal", "FREQ=WEEKLY;BYDAY=-1FR;BYHOUR=9;BYMINUTE=0", false},
		{"monthly_ordinals", "FREQ=MONTHLY;BYDAY=1MO,2TU,3WE,4TH,-1FR;BYHOUR=9;BYMINUTE=0", true},
		{"monthly_plain", "FREQ=MONTHLY;BYDAY=MO;BYHOUR=9;BYMINUTE=0", true},
		{"ordinal_zero", "FREQ=MONTHLY;BYDAY=0MO;BYHOUR=9;BYMINUTE=0", false},
		{"ordinal_five", "FREQ=MONTHLY;BYDAY=5MO;BYHOUR=9;BYMINUTE=0", false},
		{"ordinal_negative_two", "FREQ=MONTHLY;BYDAY=-2MO;BYHOUR=9;BYMINUTE=0", false},
		{"monthday_bounds", "FREQ=MONTHLY;BYMONTHDAY=1,31,-1;BYHOUR=9;BYMINUTE=0", true},
		{"monthday_daily", base + ";BYMONTHDAY=1", false},
		{"monthday_weekly", "FREQ=WEEKLY;BYMONTHDAY=1;BYHOUR=9;BYMINUTE=0", false},
		{"monthday_zero", "FREQ=MONTHLY;BYMONTHDAY=0;BYHOUR=9;BYMINUTE=0", false},
		{"monthday_large", "FREQ=MONTHLY;BYMONTHDAY=32;BYHOUR=9;BYMINUTE=0", false},
		{"monthday_negative", "FREQ=MONTHLY;BYMONTHDAY=-2;BYHOUR=9;BYMINUTE=0", false},
		{"impossible_intersection", "FREQ=MONTHLY;BYMONTHDAY=31;BYDAY=1MO;BYHOUR=9;BYMINUTE=0", false},
		{"impossible_daily_phase", base + ";INTERVAL=7;BYDAY=MO", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			p, err := schedule.Compile(schedule.Spec{RRule: tt.rule, TimeZone: "Asia/Shanghai"}, instant(t, "2026-10-28T00:00:00Z"))
			if tt.valid {
				if err != nil {
					t.Fatal(err)
				}
				if p.Spec().RRule != strings.ToUpper(p.Spec().RRule) {
					t.Fatal("规则未规范化")
				}
			} else if !errors.Is(err, schedule.ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestCompileSpec(t *testing.T) {
	start := instant(t, "2026-10-28T00:00:00Z")
	for _, tt := range []struct {
		name  string
		spec  schedule.Spec
		valid bool
	}{
		{"neither", schedule.Spec{TimeZone: "UTC"}, false},
		{"both", schedule.Spec{At: "2026-10-30T17:00", RRule: "FREQ=DAILY;BYHOUR=9;BYMINUTE=0", TimeZone: "UTC"}, false},
		{"zone_empty", schedule.Spec{At: "2026-10-30T17:00"}, false},
		{"zone_bad", schedule.Spec{At: "2026-10-30T17:00", TimeZone: "Not/A_Zone"}, false},
		{"zone_local", schedule.Spec{At: "2026-10-30T17:00", TimeZone: "Local"}, false},
		{"at_seconds", schedule.Spec{At: "2026-10-30T17:00:15", TimeZone: "UTC"}, true},
		{"at_equal", schedule.Spec{At: "2026-10-28T00:00", TimeZone: "UTC"}, false},
		{"at_past", schedule.Spec{At: "2026-10-27T23:59", TimeZone: "UTC"}, false},
		{"at_offset", schedule.Spec{At: "2026-10-30T17:00+08:00", TimeZone: "UTC"}, false},
		{"at_fraction", schedule.Spec{At: "2026-10-30T17:00:00.1", TimeZone: "UTC"}, false},
		{"at_bad_date", schedule.Spec{At: "2026-02-30T17:00", TimeZone: "UTC"}, false},
		{"until_equal_start", schedule.Spec{At: "2026-10-30T17:00", TimeZone: "UTC", Until: start}, false},
		{"until_before_at", schedule.Spec{At: "2026-10-30T17:00", TimeZone: "UTC", Until: start.Add(time.Hour)}, false},
		{"until_at", schedule.Spec{At: "2026-10-30T17:00", TimeZone: "UTC", Until: instant(t, "2026-10-30T17:00:00Z")}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := schedule.Compile(tt.spec, start)
			if tt.valid && err != nil {
				t.Fatal(err)
			}
			if !tt.valid && !errors.Is(err, schedule.ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestNext(t *testing.T) {
	for _, tt := range []struct {
		name      string
		start     string
		zone      string
		rule      string
		want      []string
		exhausted bool
	}{
		{"daily", "2026-10-28T00:00:00Z", "Asia/Shanghai", "FREQ=DAILY;BYHOUR=9,17;BYMINUTE=0", []string{"2026-10-28T01:00:00Z", "2026-10-28T09:00:00Z", "2026-10-29T01:00:00Z"}, false},
		{"weekly_days", "2026-10-28T00:00:00Z", "Asia/Shanghai", "FREQ=WEEKLY;BYDAY=MO,FR;BYHOUR=17;BYMINUTE=0", []string{"2026-10-30T09:00:00Z", "2026-11-02T09:00:00Z", "2026-11-06T09:00:00Z"}, false},
		{"weekly_default", "2026-10-28T00:00:00Z", "Asia/Shanghai", "FREQ=WEEKLY;BYHOUR=17;BYMINUTE=0", []string{"2026-10-28T09:00:00Z", "2026-11-04T09:00:00Z"}, false},
		{"weekly_anchor", "2026-10-28T00:00:00Z", "Asia/Shanghai", "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,FR;BYHOUR=17;BYMINUTE=0", []string{"2026-10-30T09:00:00Z", "2026-11-09T09:00:00Z", "2026-11-13T09:00:00Z"}, false},
		{"daily_interval", "2026-10-28T10:00:00Z", "UTC", "FREQ=DAILY;INTERVAL=2;BYHOUR=9;BYMINUTE=0", []string{"2026-10-30T09:00:00Z", "2026-11-01T09:00:00Z"}, false},
		{"month_31", "2026-01-31T10:00:00Z", "UTC", "FREQ=MONTHLY;BYMONTHDAY=31;BYHOUR=9;BYMINUTE=0", []string{"2026-03-31T09:00:00Z", "2026-05-31T09:00:00Z"}, false},
		{"month_default", "2026-01-31T10:00:00Z", "UTC", "FREQ=MONTHLY;BYHOUR=9;BYMINUTE=0", []string{"2026-03-31T09:00:00Z", "2026-05-31T09:00:00Z"}, false},
		{"month_last", "2028-01-31T10:00:00Z", "UTC", "FREQ=MONTHLY;BYMONTHDAY=-1;BYHOUR=9;BYMINUTE=0", []string{"2028-02-29T09:00:00Z", "2028-03-31T09:00:00Z"}, false},
		{"month_first_monday", "2026-10-28T00:00:00Z", "UTC", "FREQ=MONTHLY;BYDAY=1MO;BYHOUR=9;BYMINUTE=0", []string{"2026-11-02T09:00:00Z", "2026-12-07T09:00:00Z"}, false},
		{"month_last_friday", "2026-10-28T00:00:00Z", "UTC", "FREQ=MONTHLY;BYDAY=-1FR;BYHOUR=9;BYMINUTE=0", []string{"2026-10-30T09:00:00Z", "2026-11-27T09:00:00Z"}, false},
		{"month_plain_day", "2026-10-28T00:00:00Z", "UTC", "FREQ=MONTHLY;BYDAY=MO;BYHOUR=9;BYMINUTE=0", []string{"2026-11-02T09:00:00Z", "2026-11-09T09:00:00Z"}, false},
		{"month_intersection", "2026-10-28T00:00:00Z", "UTC", "FREQ=MONTHLY;BYMONTHDAY=1,2,3,4,5,6,7;BYDAY=MO;BYHOUR=9;BYMINUTE=0", []string{"2026-11-02T09:00:00Z", "2026-12-07T09:00:00Z"}, false},
		{"month_interval", "2026-01-31T10:00:00Z", "UTC", "FREQ=MONTHLY;INTERVAL=2;BYMONTHDAY=1;BYHOUR=9;BYMINUTE=0", []string{"2026-03-01T09:00:00Z", "2026-05-01T09:00:00Z"}, false},
		{"count", "2026-10-28T09:00:00Z", "UTC", "FREQ=DAILY;BYHOUR=9;BYMINUTE=0;COUNT=2", []string{"2026-10-29T09:00:00Z", "2026-10-30T09:00:00Z"}, true},
		{"until_utc", "2026-10-28T00:00:00Z", "Asia/Shanghai", "FREQ=DAILY;BYHOUR=9;BYMINUTE=0;UNTIL=20261029T010000Z", []string{"2026-10-28T01:00:00Z", "2026-10-29T01:00:00Z"}, true},
		{"until_local", "2026-10-28T00:00:00Z", "Asia/Shanghai", "FREQ=DAILY;BYHOUR=9;BYMINUTE=0;UNTIL=20261029T090000", []string{"2026-10-28T01:00:00Z", "2026-10-29T01:00:00Z"}, true},
		{"spring", "2026-03-07T12:00:00Z", "America/New_York", "FREQ=DAILY;BYHOUR=2;BYMINUTE=30", []string{"2026-03-08T07:00:00Z", "2026-03-09T06:30:00Z"}, false},
		{"fall", "2026-10-31T12:00:00Z", "America/New_York", "FREQ=DAILY;BYHOUR=1;BYMINUTE=30", []string{"2026-11-01T05:30:00Z", "2026-11-02T06:30:00Z"}, false},
		{"midnight_fall", "2026-10-31T12:00:00Z", "America/Havana", "FREQ=DAILY;BYHOUR=0;BYMINUTE=30", []string{"2026-11-01T04:30:00Z", "2026-11-02T05:30:00Z"}, false},
		{"spring_deduplicate", "2026-03-07T12:00:00Z", "America/New_York", "FREQ=DAILY;BYHOUR=2,3;BYMINUTE=0;COUNT=2", []string{"2026-03-08T07:00:00Z", "2026-03-09T06:00:00Z"}, true},
		{"date_skip", "2011-12-29T12:00:00Z", "Pacific/Apia", "FREQ=DAILY;BYHOUR=0;BYMINUTE=0;COUNT=2", []string{"2011-12-30T10:00:00Z", "2011-12-31T10:00:00Z"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := compile(t, schedule.Spec{RRule: tt.rule, TimeZone: tt.zone}, tt.start)
			after := instant(t, tt.start)
			for _, expected := range tt.want {
				got, ok := p.Next(after)
				if !ok || !got.Equal(instant(t, expected)) || got.Location() != time.UTC {
					t.Fatalf("after %s: got %s, %v; want %s UTC", after, got, ok, expected)
				}
				after = got
			}
			if tt.exhausted {
				if got, ok := p.Next(after); ok || !got.IsZero() {
					t.Fatalf("not exhausted: %s", got)
				}
			}
		})
	}
}

func TestOneShot(t *testing.T) {
	for _, tt := range []struct{ name, zone, start, at, want string }{
		{"shanghai", "Asia/Shanghai", "2026-10-28T00:00:00Z", "2026-10-30T17:00", "2026-10-30T09:00:00Z"},
		{"seconds", "UTC", "2026-10-28T00:00:00Z", "2026-10-30T17:00:15", "2026-10-30T17:00:15Z"},
		{"spring", "America/New_York", "2026-03-07T12:00:00Z", "2026-03-08T02:30", "2026-03-08T07:00:00Z"},
		{"fall", "America/New_York", "2026-10-31T12:00:00Z", "2026-11-01T01:30", "2026-11-01T05:30:00Z"},
		{"half_hour_gap", "Australia/Lord_Howe", "2026-10-03T00:00:00Z", "2026-10-04T02:15", "2026-10-03T15:30:00Z"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := compile(t, schedule.Spec{At: tt.at, TimeZone: tt.zone}, tt.start)
			want := instant(t, tt.want)
			if !p.OneShot() {
				t.Fatal("not one shot")
			}
			for _, after := range []time.Time{p.Start(), want.Add(-time.Nanosecond)} {
				got, ok := p.Next(after)
				if !ok || !got.Equal(want) {
					t.Fatalf("got %s %v, want %s", got, ok, want)
				}
			}
			for _, after := range []time.Time{want, want.Add(time.Hour)} {
				if got, ok := p.Next(after); ok || !got.IsZero() {
					t.Fatal("一次性不应再次到期")
				}
			}
		})
	}
	// 回拨的第二次 01:30 不得让已经过去的第一次重新变成未来。
	_, err := schedule.Compile(schedule.Spec{At: "2026-11-01T01:30", TimeZone: "America/New_York"}, instant(t, "2026-11-01T06:00:00Z"))
	if !errors.Is(err, schedule.ErrInvalid) {
		t.Fatalf("got %v", err)
	}
}

func TestLimits(t *testing.T) {
	for _, tt := range []struct{ name, rule, until, want string }{
		{"spec_only", "FREQ=DAILY;BYHOUR=9;BYMINUTE=0", "2026-10-29T09:00:00Z", "2026-10-29T09:00:00Z"},
		{"spec_earlier", "FREQ=DAILY;BYHOUR=9;BYMINUTE=0;UNTIL=20261030T090000Z", "2026-10-29T09:00:00Z", "2026-10-29T09:00:00Z"},
		{"rrule_earlier", "FREQ=DAILY;BYHOUR=9;BYMINUTE=0;UNTIL=20261029T090000Z", "2026-10-30T09:00:00Z", "2026-10-29T09:00:00Z"},
		{"count_spec", "FREQ=DAILY;BYHOUR=9;BYMINUTE=0;COUNT=1", "2026-10-30T09:00:00Z", "2026-10-28T09:00:00Z"},
		{"local_gap_until", "FREQ=DAILY;BYHOUR=2;BYMINUTE=30;UNTIL=20260308T023000", "2026-03-10T00:00:00Z", "2026-03-08T07:00:00Z"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			start, zone := "2026-10-28T00:00:00Z", "UTC"
			if tt.name == "local_gap_until" {
				start, zone = "2026-03-07T12:00:00Z", "America/New_York"
			}
			p := compile(t, schedule.Spec{RRule: tt.rule, TimeZone: zone, Until: instant(t, tt.until)}, start)
			got := p.Catchup(p.Start(), instant(t, "2026-12-01T00:00:00Z"))
			if !got.Fire.Equal(instant(t, tt.want)) || !got.Next.IsZero() {
				t.Fatalf("got %+v", got)
			}
		})
	}
	t.Run("impossible_month_phase", func(t *testing.T) {
		_, err := schedule.Compile(schedule.Spec{RRule: "FREQ=MONTHLY;INTERVAL=12;BYMONTHDAY=31;BYHOUR=9;BYMINUTE=0", TimeZone: "UTC"}, instant(t, "2026-04-01T00:00:00Z"))
		if !errors.Is(err, schedule.ErrInvalid) {
			t.Fatalf("got %v", err)
		}
	})
	for _, tt := range []struct {
		name  string
		start string
		spec  schedule.Spec
	}{
		{
			name:  "search_horizon",
			start: "2026-01-01T00:00:00Z",
			// 四年一次的一月 31 日与星期一取交集，首次到期在 2050 年。
			spec: schedule.Spec{RRule: "FREQ=MONTHLY;INTERVAL=48;BYMONTHDAY=31;BYDAY=-1MO;BYHOUR=9;BYMINUTE=0", TimeZone: "UTC"},
		},
		{
			name:  "rrule_until_before_first",
			start: "2026-10-28T00:00:00Z",
			spec:  schedule.Spec{RRule: "FREQ=DAILY;BYHOUR=9;BYMINUTE=0;UNTIL=20261028T080000Z", TimeZone: "UTC"},
		},
		{
			name:  "spec_until_before_first",
			start: "2026-10-28T00:00:00Z",
			spec:  schedule.Spec{RRule: "FREQ=DAILY;BYHOUR=9;BYMINUTE=0", TimeZone: "UTC", Until: instant(t, "2026-10-28T08:00:00Z")},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, err := schedule.Compile(tt.spec, instant(t, tt.start))
			if p != nil || !errors.Is(err, schedule.ErrInvalid) {
				t.Fatalf("want nil Plan and ErrInvalid, got %v, %v", p, err)
			}
			want := schedule.ErrInvalid.Error() + ": 这个规则在十年内没有任何一次执行，请检查 BYMONTHDAY、BYDAY、INTERVAL 与 until/COUNT"
			if err.Error() != want {
				t.Fatalf("got %q, want %q", err.Error(), want)
			}
		})
	}
}

func TestCatchup(t *testing.T) {
	start := "2026-10-28T00:00:00Z"
	for _, tt := range []struct {
		name, rule, last, now, fire, first, skippedLast, next string
		count                                                 int64
	}{
		{"none", "", start, "2026-10-28T08:00:00Z", "", "", "", "2026-10-28T09:00:00Z", 0},
		{"one", "", start, "2026-10-28T10:00:00Z", "2026-10-28T09:00:00Z", "", "", "2026-10-29T09:00:00Z", 0},
		{"exact", "", start, "2026-10-28T09:00:00Z", "2026-10-28T09:00:00Z", "", "", "2026-10-29T09:00:00Z", 0},
		{"many", "", start, "2026-10-31T09:00:00Z", "2026-10-31T09:00:00Z", "2026-10-28T09:00:00Z", "2026-10-30T09:00:00Z", "2026-11-01T09:00:00Z", 3},
		{"last_excluded", "", "2026-10-29T09:00:00Z", "2026-10-31T09:00:00Z", "2026-10-31T09:00:00Z", "2026-10-30T09:00:00Z", "2026-10-30T09:00:00Z", "2026-11-01T09:00:00Z", 1},
		{"already_processed", "", "2026-10-29T09:00:00Z", "2026-10-29T10:00:00Z", "", "", "", "2026-10-30T09:00:00Z", 0},
		{"count_includes_skipped", "FREQ=DAILY;BYHOUR=9;BYMINUTE=0;COUNT=3", start, "2026-11-10T00:00:00Z", "2026-10-30T09:00:00Z", "2026-10-28T09:00:00Z", "2026-10-29T09:00:00Z", "", 2},
		{"count_after_processed", "FREQ=DAILY;BYHOUR=9;BYMINUTE=0;COUNT=3", "2026-10-29T09:00:00Z", "2026-11-10T00:00:00Z", "2026-10-30T09:00:00Z", "", "", "", 0},
		{"count_exhausted", "FREQ=DAILY;BYHOUR=9;BYMINUTE=0;COUNT=3", "2026-10-30T09:00:00Z", "2026-11-10T00:00:00Z", "", "", "", "", 0},
		{"within_day", "FREQ=DAILY;BYHOUR=9,10,11;BYMINUTE=0", start, "2026-10-28T11:30:00Z", "2026-10-28T11:00:00Z", "2026-10-28T09:00:00Z", "2026-10-28T10:00:00Z", "2026-10-29T09:00:00Z", 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rule := tt.rule
			if rule == "" {
				rule = "FREQ=DAILY;BYHOUR=9;BYMINUTE=0"
			}
			p := compile(t, schedule.Spec{RRule: rule, TimeZone: "UTC"}, start)
			parse := func(s string) time.Time {
				if s == "" {
					return time.Time{}
				}
				return instant(t, s)
			}
			want := schedule.Catchup{Fire: parse(tt.fire), Next: parse(tt.next), Skipped: schedule.Skipped{First: parse(tt.first), Last: parse(tt.skippedLast), Count: tt.count}}
			got := p.Catchup(parse(tt.last), parse(tt.now))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v; want %+v", got, want)
			}
		})
	}
	t.Run("one_shot", func(t *testing.T) {
		p := compile(t, schedule.Spec{At: "2026-10-28T09:00", TimeZone: "UTC"}, start)
		got := p.Catchup(p.Start(), instant(t, "2026-11-01T00:00:00Z"))
		if !got.Fire.Equal(instant(t, "2026-10-28T09:00:00Z")) || !got.Next.IsZero() || got.Skipped.Count != 0 {
			t.Fatalf("got %+v", got)
		}
		if got := p.Catchup(got.Fire, instant(t, "2026-11-01T00:00:00Z")); !got.Fire.IsZero() {
			t.Fatal("重复执行")
		}
	})
}

func hourlyRule() string {
	var hours []string
	for i := 0; i < 24; i++ {
		hours = append(hours, fmt.Sprint(i))
	}
	return "FREQ=DAILY;BYHOUR=" + strings.Join(hours, ",") + ";BYMINUTE=0"
}

func TestCatchupTwoYears(t *testing.T) {
	p := compile(t, schedule.Spec{RRule: hourlyRule(), TimeZone: "UTC"}, "2024-01-01T00:00:00Z")
	now := instant(t, "2026-01-01T00:00:00Z")
	begin := time.Now()
	got := p.Catchup(p.Start(), now)
	elapsed := time.Since(begin)
	if !got.Fire.Equal(now) || got.Skipped.Count != 17543 || !got.Skipped.First.Equal(p.Start().Add(time.Hour)) || !got.Skipped.Last.Equal(now.Add(-time.Hour)) || !got.Next.Equal(now.Add(time.Hour)) {
		t.Fatalf("got %+v", got)
	}
	// 留出 race 与共享 CI 的余量；基准测试另行输出实际毫秒数。
	if elapsed > 250*time.Millisecond {
		t.Fatalf("两年追赶耗时 %s", elapsed)
	}
}

func TestCatchupBeyondEnumerationLimit(t *testing.T) {
	p := compile(t, schedule.Spec{RRule: hourlyRule(), TimeZone: "UTC"}, "2000-01-01T00:00:00Z")
	now := instant(t, "2026-01-01T00:00:00Z")
	got := p.Catchup(p.Start(), now)
	want := int64(now.Sub(p.Start())/time.Hour) - 1
	if want <= 100000 || got.Skipped.Count != want || !got.Fire.Equal(now) {
		t.Fatalf("got %+v, want skipped %d", got, want)
	}
}

func BenchmarkCatchupTwoYears(b *testing.B) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	p, err := schedule.Compile(schedule.Spec{RRule: hourlyRule(), TimeZone: "America/New_York"}, start)
	if err != nil {
		b.Fatal(err)
	}
	now := start.AddDate(2, 0, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Catchup(start, now)
	}
}

func TestFormatLocal(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ value, ref, want string }{
		{"2026-10-30T09:00:00Z", "2026-10-28T00:00:00Z", "10月30日星期五 17:00"},
		{"2027-01-02T01:00:00Z", "2026-10-28T00:00:00Z", "2027年1月2日星期六 09:00"},
		{"2027-01-02T01:00:00Z", "2026-12-31T16:00:00Z", "1月2日星期六 09:00"},
		{"2026-11-01T01:00:00Z", "2026-10-28T00:00:00Z", "11月1日星期日 09:00"},
	} {
		if got := schedule.FormatLocal(instant(t, tt.value), loc, instant(t, tt.ref)); got != tt.want {
			t.Fatalf("got %q; want %q", got, tt.want)
		}
	}
}

func TestDeterminismAndCatchupAgainstNext(t *testing.T) {
	for seed := int64(0); seed < 12; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(rng.Intn(300)) * 24 * time.Hour)
			zone := []string{"UTC", "Asia/Shanghai", "America/New_York"}[rng.Intn(3)]
			rule := []string{"FREQ=DAILY;BYHOUR=2,3,9;BYMINUTE=0", "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,FR;BYHOUR=17;BYMINUTE=0", "FREQ=MONTHLY;BYMONTHDAY=1,-1;BYHOUR=9;BYMINUTE=0;COUNT=12"}[rng.Intn(3)]
			p, err := schedule.Compile(schedule.Spec{RRule: rule, TimeZone: zone}, start)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(p.Spec())
			if err != nil {
				t.Fatal(err)
			}
			var spec schedule.Spec
			if err := json.Unmarshal(encoded, &spec); err != nil {
				t.Fatal(err)
			}
			copyPlan, err := schedule.Compile(spec, p.Start())
			if err != nil {
				t.Fatal(err)
			}
			if p.OneShot() || p.Loc().String() != zone {
				t.Fatal("访问接口不符")
			}
			now := start.AddDate(0, 3, 0)
			var due []time.Time
			cursor := start
			var next time.Time
			for {
				value, ok := p.Next(cursor)
				if !ok {
					break
				}
				if value.After(now) {
					next = value
					break
				}
				due = append(due, value)
				cursor = value
			}
			want := schedule.Catchup{Next: next}
			if len(due) > 0 {
				want.Fire = due[len(due)-1]
			}
			if len(due) > 1 {
				want.Skipped = schedule.Skipped{First: due[0], Last: due[len(due)-2], Count: int64(len(due) - 1)}
			}
			for _, got := range []schedule.Catchup{p.Catchup(start, now), p.Catchup(start, now), copyPlan.Catchup(start, now)} {
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("seed %d: got %+v, want %+v", seed, got, want)
				}
			}
			before, ok := p.Next(start.Add(-time.Hour))
			after, afterOK := p.Next(start)
			if ok != afterOK || !before.Equal(after) {
				t.Fatal("不能在创建之前执行")
			}
		})
	}
}

func TestCanonicalSpec(t *testing.T) {
	p := compile(t, schedule.Spec{RRule: "freq=monthly;interval=02;byday=01mo,mo,01mo;byhour=17,09,17;byminute=00;count=02;BYMONTHDAY=31,01,31", TimeZone: "UTC"}, "2026-01-01T00:00:00Z")
	want := "BYDAY=1MO,MO;BYHOUR=9,17;BYMINUTE=0;BYMONTHDAY=1,31;COUNT=2;FREQ=MONTHLY;INTERVAL=2"
	if p.Spec().RRule != want {
		t.Fatalf("got %s, want %s", p.Spec().RRule, want)
	}
}

func TestCatchupTransitions(t *testing.T) {
	for _, tt := range []struct {
		name, zone, start, now, rule, fire, first, last, next string
		count                                                 int64
	}{
		{"spring", "America/New_York", "2026-03-07T12:00:00Z", "2026-03-09T12:00:00Z", "FREQ=DAILY;BYHOUR=2,3;BYMINUTE=0;COUNT=3", "2026-03-09T07:00:00Z", "2026-03-08T07:00:00Z", "2026-03-09T06:00:00Z", "", 2},
		{"fall", "America/New_York", "2026-10-31T12:00:00Z", "2026-11-01T06:45:00Z", "FREQ=DAILY;BYHOUR=1;BYMINUTE=30", "2026-11-01T05:30:00Z", "", "", "2026-11-02T06:30:00Z", 0},
		{"date_skip", "Pacific/Apia", "2011-12-29T12:00:00Z", "2012-01-01T00:00:00Z", "FREQ=DAILY;BYHOUR=0;BYMINUTE=0;COUNT=2", "2011-12-31T10:00:00Z", "2011-12-30T10:00:00Z", "2011-12-30T10:00:00Z", "", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := compile(t, schedule.Spec{RRule: tt.rule, TimeZone: tt.zone}, tt.start)
			parse := func(s string) time.Time {
				if s == "" {
					return time.Time{}
				}
				return instant(t, s)
			}
			want := schedule.Catchup{Fire: parse(tt.fire), Next: parse(tt.next), Skipped: schedule.Skipped{First: parse(tt.first), Last: parse(tt.last), Count: tt.count}}
			got := p.Catchup(p.Start(), parse(tt.now))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v; want %+v", got, want)
			}
		})
	}
}
