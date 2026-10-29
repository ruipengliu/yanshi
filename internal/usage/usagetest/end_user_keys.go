package usagetest

import (
	"context"
	"testing"
	"time"

	"yanshi/internal/ids"
	"yanshi/internal/usage"
)

func endUserKeys(t *testing.T, s usage.Store) {
	ctx := context.Background()
	first := entry("same", "a", "owner", 10, t0)
	if err := s.Record(ctx, first); err != nil {
		t.Fatal(err)
	}
	// 历史用量 ID 全局幂等；本步不把重复 ID 放宽为另一个用户的新费用。
	if err := s.Record(ctx, entry("same", "a", "other", 100, t0)); err != nil {
		t.Fatal(err)
	}
	if err := s.AnonymizeEntry(ctx, "other", "same"); err != nil {
		t.Fatal(err)
	}
	end := t0.Add(time.Hour)
	if got := spent(t, s, "a", "owner", t0, end); got != 10 {
		t.Fatalf("wrong EndUser changed owner's usage: %d", got)
	}
	if got := spent(t, s, "a", "other", t0, end); got != 0 {
		t.Fatalf("duplicate global ID reassigned usage: %d", got)
	}
}

func legacyKeys(t *testing.T, s usage.Store, newFirst, anonymousFirst bool) {
	ctx := context.Background()
	old := entry("call_meter/1", "a", "owner", 10, t0)
	next := *old
	var err error
	next.ID, err = ids.WithEndUser("use", "encoded-call-meter-1", old.EndUser)
	if err != nil {
		t.Fatal(err)
	}
	next.LegacyID = old.ID
	first, retry := old, &next
	if newFirst {
		first, retry = retry, first
	}
	if err := s.Record(ctx, first); err != nil {
		t.Fatal(err)
	}
	if anonymousFirst {
		if err := s.DeleteEndUser(ctx, "a", old.EndUser); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []*usage.Entry{retry, first, &next, old} {
		if err := s.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	// Meter 用原幂等键撤回；新 ID 重试命中旧行时同样清掉旧行的归属。
	if err := s.AnonymizeEntry(ctx, "wrong", next.DedupKey()); err != nil {
		t.Fatal(err)
	}
	if !anonymousFirst && spent(t, s, "a", old.EndUser, t0, t0.Add(time.Hour)) != 10 {
		t.Fatal("wrong EndUser anonymized a legacy alias")
	}
	if err := s.AnonymizeEntry(ctx, old.EndUser, next.DedupKey()); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, &next); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Report(ctx, usage.Query{BusinessLine: "a", From: t0, To: t0.Add(time.Hour), GroupBy: usage.ByEndUser})
	if err != nil || len(rows) != 1 || rows[0].Key != usage.Anonymous || rows[0].Calls != 1 || rows[0].Cost != 10 {
		t.Fatalf("new/old replay changed totals or revived owner: %+v, %v", rows, err)
	}
}
