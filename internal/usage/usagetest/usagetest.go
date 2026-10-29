// Package usagetest 是 usage.Store 实现的一致性测试套件。
package usagetest

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"yanshi/internal/usage"
)

var t0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

func entry(id, bl, eu string, cost int64, at time.Time) *usage.Entry {
	return &usage.Entry{ID: id, BusinessLine: bl, EndUser: eu, Kind: usage.Model, Model: "p/m", Agent: "a@" + eu, InputTokens: 10, CachedInputTokens: 6, OutputTokens: 2, Cost: cost, At: at}
}

func spent(t *testing.T, s usage.Store, bl, eu string, from, to time.Time) int64 {
	t.Helper()
	v, err := s.Spent(context.Background(), bl, eu, from, to)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func Run(t *testing.T, newStore func(t *testing.T) usage.Store) {
	t.Run("EndUserKeys", func(t *testing.T) { endUserKeys(t, newStore(t)) })
	for _, newFirst := range []bool{false, true} {
		for _, anonymousFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("LegacyKeys/newFirst=%v/anonymousFirst=%v", newFirst, anonymousFirst), func(t *testing.T) {
				legacyKeys(t, newStore(t), newFirst, anonymousFirst)
			})
		}
	}
	ctx := context.Background()

	t.Run("RecordIsIdempotent", func(t *testing.T) {
		s := newStore(t)
		e := entry("u1", "a", "x", 100, t0.Add(10*time.Minute))
		for range 3 {
			if err := s.Record(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		if got := spent(t, s, "a", "x", t0, t0.Add(time.Hour)); got != 100 {
			t.Fatalf("spent %d after duplicate records, want 100", got)
		}
		if got := spent(t, s, "a", "", t0, t0.Add(time.Hour)); got != 100 {
			t.Fatalf("business line spent %d, want 100", got)
		}
	})

	t.Run("RejectsInvalid", func(t *testing.T) {
		s := newStore(t)
		for _, e := range []*usage.Entry{entry("", "a", "x", 1, t0), entry("i", "a", "", 1, t0), entry("i", "a", usage.Anonymous, 1, t0), entry("i", "a", "x", -1, t0)} {
			if err := s.Record(ctx, e); err == nil {
				t.Fatalf("accepted invalid entry %+v", e)
			}
		}
	})

	t.Run("SpentRangesAndScopes", func(t *testing.T) {
		s := newStore(t)
		for i, e := range []*usage.Entry{
			entry("e1", "a", "x", 1, t0.Add(59*time.Minute)),
			entry("e2", "a", "x", 10, t0.Add(time.Hour)),
			entry("e3", "a", "y", 100, t0.Add(90*time.Minute)),
			entry("e4", "b", "x", 1000, t0.Add(90*time.Minute)),
			entry("e5", "a", "x", 10000, t0.Add(-time.Second)),
		} {
			if err := s.Record(ctx, e); err != nil {
				t.Fatal(i, err)
			}
		}
		cases := []struct {
			bl, eu   string
			from, to time.Time
			want     int64
		}{
			{"a", "x", t0, t0.Add(time.Hour), 1},
			{"a", "x", t0, t0.Add(2 * time.Hour), 11},
			{"a", "", t0, t0.Add(2 * time.Hour), 111},
			{"a", "", t0.Add(-time.Hour), t0.Add(2 * time.Hour), 10111},
			{"b", "", t0, t0.Add(2 * time.Hour), 1000},
			{"a", "z", t0, t0.Add(2 * time.Hour), 0},
		}
		for _, c := range cases {
			if got := spent(t, s, c.bl, c.eu, c.from, c.to); got != c.want {
				t.Errorf("Spent(%s, %q, %s, %s) = %d, want %d", c.bl, c.eu, c.from, c.to, got, c.want)
			}
		}
	})

	t.Run("Report", func(t *testing.T) {
		s := newStore(t)
		// 使用 IANA 时区名：PostgreSQL 按名称换算，"UTC+8" 之类的名称在 POSIX 语义下符号相反。
		cst, err := time.LoadLocation("Asia/Shanghai")
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Record(ctx, entry("e1", "a", "x", 1, t0.Add(15*time.Hour))) // UTC+8 下为 3 月 1 日 23 点
		_ = s.Record(ctx, entry("e2", "a", "x", 2, t0.Add(17*time.Hour))) // 3 月 2 日 1 点
		_ = s.Record(ctx, entry("e3", "a", "y", 4, t0.Add(17*time.Hour)))
		_ = s.Record(ctx, &usage.Entry{ID: "e4", BusinessLine: "a", EndUser: "y", Kind: usage.Sandbox, Model: "sandbox", SandboxMillis: 1500, Cost: 8, At: t0.Add(17 * time.Hour)})
		_ = s.Record(ctx, entry("e5", "b", "x", 16, t0.Add(17*time.Hour)))

		q := usage.Query{BusinessLine: "a", From: t0, To: t0.Add(48 * time.Hour), Location: cst}
		check := func(by usage.GroupBy, eu string, want string) {
			t.Helper()
			q := q
			q.GroupBy, q.EndUser = by, eu
			rows, err := s.Report(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(rows); got != want {
				t.Errorf("Report(%s, %q) = %s, want %s", by, eu, got, want)
			}
		}
		check(usage.ByDay, "", "[{2026-03-01 1 1 10 6 2 0} {2026-03-02 3 14 20 12 4 1500}]")
		check(usage.ByEndUser, "", "[{x 2 3 20 12 4 0} {y 2 12 10 6 2 1500}]")
		check(usage.ByModel, "", "[{p/m 3 7 30 18 6 0} {sandbox 1 8 0 0 0 1500}]")
		check(usage.ByModel, "y", "[{p/m 1 4 10 6 2 0} {sandbox 1 8 0 0 0 1500}]")
		check(usage.ByAgent, "", "[{ 1 8 0 0 0 1500} {a@x 2 3 20 12 4 0} {a@y 1 4 10 6 2 0}]")
	})

	t.Run("DeleteEndUserKeepsTotals", func(t *testing.T) {
		s := newStore(t)
		_ = s.Record(ctx, entry("e1", "a", "x", 1, t0))
		_ = s.Record(ctx, entry("e2", "a", "x", 2, t0.Add(3*time.Hour)))
		_ = s.Record(ctx, entry("e3", "a", "y", 4, t0))
		_ = s.Record(ctx, entry("e4", "b", "x", 8, t0))
		// 另一个已注销用户的匿名用量，与本次匿名化的用量合并。
		_ = s.Record(ctx, entry("e5", "a", "w", 16, t0))
		if err := s.DeleteEndUser(ctx, "a", "w"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteEndUser(ctx, "a", "x"); err != nil {
			t.Fatal(err)
		}
		end := t0.Add(24 * time.Hour)
		if got := spent(t, s, "a", "x", t0, end); got != 0 {
			t.Fatalf("deleted end user still has %d", got)
		}
		if got := spent(t, s, "a", usage.Anonymous, t0, end); got != 19 {
			t.Fatalf("anonymous spent %d, want 19", got)
		}
		if got := spent(t, s, "a", "", t0, end); got != 23 {
			t.Fatalf("business line total %d after anonymization, want 23", got)
		}
		if got := spent(t, s, "b", "x", t0, end); got != 8 {
			t.Fatalf("other business line affected: %d", got)
		}
		rows, _ := s.Report(ctx, usage.Query{BusinessLine: "a", From: t0, To: end, GroupBy: usage.ByEndUser})
		if keys := keysOf(rows); !slices.Equal(keys, []string{"y", usage.Anonymous}) {
			t.Fatalf("report end users %v", keys)
		}
		// 匿名化后同一 ID 的重复记录仍是幂等的，不会以原 EndUser 再出现。
		_ = s.Record(ctx, entry("e1", "a", "x", 1, t0))
		if got := spent(t, s, "a", "x", t0, end); got != 0 {
			t.Fatalf("re-record after anonymization resurrected end user: %d", got)
		}
		if err := s.DeleteEndUser(ctx, "a", ""); err == nil {
			t.Fatal("anonymized the business line total")
		}
	})

	t.Run("AnonymizeEntry", func(t *testing.T) {
		s := newStore(t)
		_ = s.Record(ctx, entry("e1", "a", "x", 1, t0))
		_ = s.Record(ctx, entry("e2", "a", "x", 2, t0))
		for range 2 { // 幂等
			if err := s.AnonymizeEntry(ctx, "x", "e2"); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.AnonymizeEntry(ctx, "x", "missing"); err != nil {
			t.Fatal(err)
		}
		end := t0.Add(time.Hour)
		if x, anon, all := spent(t, s, "a", "x", t0, end), spent(t, s, "a", usage.Anonymous, t0, end), spent(t, s, "a", "", t0, end); x != 1 || anon != 2 || all != 3 {
			t.Fatalf("after anonymizing one entry: x %d, anonymous %d, total %d; want 1, 2, 3", x, anon, all)
		}
		rows, _ := s.Report(ctx, usage.Query{BusinessLine: "a", From: t0, To: end, GroupBy: usage.ByEndUser})
		if keys := keysOf(rows); !slices.Equal(keys, []string{"x", usage.Anonymous}) {
			t.Fatalf("report end users %v", keys)
		}
	})

	t.Run("OneshotAttribution", func(t *testing.T) {
		s := newStore(t)
		for i, label := range []string{"brief@1", "brief@1", "brief@2", ""} {
			e := entry(fmt.Sprintf("oneshot-%d", i), "a", "x", 10, t0)
			if label != "" {
				e.Agent, e.Oneshot = "", label
			}
			if err := s.Record(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		q := usage.Query{BusinessLine: "a", From: t0, To: t0.Add(time.Hour), GroupBy: usage.ByOneshot}
		rows, err := s.Report(ctx, q)
		if err != nil || len(rows) != 3 || rows[0].Key != "" || rows[1].Key != "brief@1" || rows[1].Calls != 2 || rows[1].CachedInputTokens != 12 || rows[2].Key != "brief@2" {
			t.Fatalf("oneshot report: %+v, %v", rows, err)
		}
		q.GroupBy = usage.ByAgent
		rows, err = s.Report(ctx, q)
		if err != nil || len(rows) != 2 || rows[0].Key != "" || rows[0].Calls != 3 || rows[1].Key != "a@x" {
			t.Fatalf("agent report: %+v, %v", rows, err)
		}
		e := entry("invalid-attribution", "a", "x", 10, t0)
		e.Oneshot = "brief@1"
		if err := s.Record(ctx, e); err == nil {
			t.Fatal("accepted both Agent and Oneshot")
		}
		if err := s.DeleteEndUser(ctx, "a", "x"); err != nil {
			t.Fatal(err)
		}
		q.GroupBy = usage.ByOneshot
		rows, err = s.Report(ctx, q)
		if err != nil || len(rows) != 3 || rows[1].Key != "brief@1" || rows[1].Calls != 2 {
			t.Fatalf("anonymization lost attribution: %+v %v", rows, err)
		}
	})

	t.Run("Prune", func(t *testing.T) {
		s := newStore(t)
		_ = s.Record(ctx, entry("old", "a", "x", 1, t0))
		_ = s.Record(ctx, entry("edge", "a", "x", 2, t0.Add(time.Hour+time.Minute)))
		_ = s.Record(ctx, entry("new", "a", "x", 4, t0.Add(3*time.Hour)))
		if err := s.Prune(ctx, t0.Add(time.Hour+30*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if got := spent(t, s, "a", "", t0.Add(-time.Hour), t0.Add(24*time.Hour)); got != 6 {
			t.Fatalf("spent %d after prune, want 6 (entries in the cut-off hour are kept)", got)
		}
		rows, _ := s.Report(ctx, usage.Query{BusinessLine: "a", From: t0.Add(-time.Hour), To: t0.Add(24 * time.Hour), GroupBy: usage.ByEndUser})
		if len(rows) != 1 || rows[0].Calls != 2 {
			t.Fatalf("report after prune %v", rows)
		}
	})
}

func keysOf(rows []usage.Row) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Key)
	}
	return out
}
