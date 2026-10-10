package usage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/lifecycle"
	"yanshi/internal/usage"
	"yanshi/internal/usage/usagetest"
)

func TestMem(t *testing.T) {
	usagetest.Run(t, func(*testing.T) usage.Store { return usage.NewMem() })
}

func TestPrices(t *testing.T) {
	p := &usage.PriceList{Models: map[string]usage.Price{"p/m": {Input: 1, Output: 4}}}
	p.Sandbox.PerHour = 0.36
	if got := p.ModelCost("p/m", 1000, 0, 500); got != 3000 {
		t.Fatalf("model cost %d, want 3000 micros", got)
	}
	// 未配置缓存单价时缓存命中按普通输入计。
	if got := p.ModelCost("p/m", 1000, 800, 500); got != 3000 {
		t.Fatalf("cached input without a cached price %d, want 3000", got)
	}
	cached := 0.25
	p.Models["p/m"] = usage.Price{Input: 1, Output: 4, CachedInput: &cached}
	if got := p.ModelCost("p/m", 1000, 800, 500); got != 200+200+2000 {
		t.Fatalf("cached input cost %d, want 2400", got)
	}
	if got := p.ModelCost("p/other", 1000, 0, 500); got != 0 {
		t.Fatalf("unpriced model cost %d", got)
	}
	if got := p.SandboxCost(10 * time.Second); got != 1000 {
		t.Fatalf("sandbox cost %d, want 1000 micros", got)
	}
}

func TestQuotas(t *testing.T) {
	ctx := context.Background()
	store := usage.NewMem()
	l := usage.Limits{Monthly: 1, EndUserDaily: 0.1}
	if err := l.Prepare(); err != nil {
		t.Fatal(err)
	}
	q := &usage.Quotas{Store: store, Limits: map[string]usage.Limits{"a": l}}
	// 北京时间 3 月 31 日 23:30。
	now := time.Date(2026, 3, 31, 15, 30, 0, 0, time.UTC)
	rec := func(id, eu string, cost int64) {
		if err := store.Record(ctx, &usage.Entry{ID: id, BusinessLine: "a", EndUser: eu, Kind: usage.Model, Cost: cost, At: now}); err != nil {
			t.Fatal(err)
		}
	}
	if p, _ := q.Check(ctx, "a", "x", now); p != nil {
		t.Fatalf("exceeded with no usage: %+v", p)
	}
	rec("1", "x", 100_000)
	p, err := q.Check(ctx, "a", "x", now)
	if err != nil || p == nil || p.Scope != "end_user" || p.Reason() != "end_user_quota" {
		t.Fatalf("end user quota not enforced: %+v %v", p, err)
	}
	if want := time.Date(2026, 3, 31, 16, 0, 0, 0, time.UTC); !p.ResetAt.Equal(want) {
		t.Fatalf("daily reset %s, want %s (midnight in Asia/Shanghai)", p.ResetAt, want)
	}
	if p, _ := q.Check(ctx, "a", "y", now); p != nil {
		t.Fatalf("other end user blocked: %+v", p)
	}
	rec("2", "y", 900_000)
	p, _ = q.Check(ctx, "a", "x", now)
	if p == nil || p.Scope != "business_line" || !p.ResetAt.Equal(time.Date(2026, 3, 31, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("both exceeded: want the later reset (month end = day end here), got %+v", p)
	}
	// 第二天（也是下个月）两层都重置。
	if p, _ := q.Check(ctx, "a", "x", now.Add(time.Hour)); p != nil {
		t.Fatalf("quota did not reset: %+v", p)
	}
	if p, _ := q.Check(ctx, "other", "x", now); p != nil || q.Enabled("other") {
		t.Fatal("business line without quota is limited")
	}
	var nilQ *usage.Quotas
	if nilQ.Enabled("a") {
		t.Fatal("nil quotas enabled")
	}
	if !errors.Is(&usage.ExceededError{Period: p0()}, usage.ErrExceeded) {
		t.Fatal("ExceededError does not unwrap to ErrExceeded")
	}
}

func p0() *usage.Period { return &usage.Period{Scope: "end_user"} }

type deletions struct {
	lifecycle.Deletions
	t *lifecycle.Tombstone
}

func (d deletions) Get(context.Context, string) (*lifecycle.Tombstone, error) {
	if d.t == nil {
		return nil, lifecycle.ErrNotFound
	}
	return d.t, nil
}

// TestMeterAnonymizesAfterDeletion：Session 删除后（例如注销账号与在途计量竞争）才写入的用量被匿名化。
func TestMeterAnonymizesAfterDeletion(t *testing.T) {
	ctx := context.Background()
	store := usage.NewMem()
	s := usage.Scope{SessionID: "s1", BusinessLine: "a", EndUser: "x"}
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	m := &usage.Meter{Store: store, Deletions: deletions{}}
	m.Model(ctx, s, "1", "a@1", "p/m", &v1.Usage{InputTokens: 1}, at)
	if !store.EndUsers()[[2]string{"a", "x"}] {
		t.Fatal("usage not recorded")
	}
	// Session 已删除（不论原因）后写入的用量被匿名化，之前的保留原 EndUser。
	m.Deletions = deletions{t: &lifecycle.Tombstone{SessionID: "s1", Reason: "end_user"}}
	m.Sandbox(ctx, s, "call_1", time.Second, at)
	rows, _ := store.Report(ctx, usage.Query{BusinessLine: "a", From: at, To: at.Add(time.Hour), GroupBy: usage.ByEndUser})
	if len(rows) != 2 || rows[0].Key != "x" || rows[0].Calls != 1 || rows[1].Key != usage.Anonymous || rows[1].Calls != 1 {
		t.Fatalf("late usage after session deletion: %+v", rows)
	}
}
