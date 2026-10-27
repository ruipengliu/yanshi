package usage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/lifecycle"
	"yanshi/internal/usage"
)

func TestEndUserBarrierAnonymizesUsageWithoutSession(t *testing.T) {
	ctx := context.Background()
	d, store := lifecycle.NewMemDeletions(), usage.NewMem()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	if _, err := d.MarkEndUser(ctx, &lifecycle.EndUserTombstone{BusinessLine: "bl", EndUser: "u", RequestID: "r", RequestedAt: now}); err != nil {
		t.Fatal(err)
	}
	meter := &usage.Meter{Store: store, Deletions: d, Prices: &usage.PriceList{}}
	meter.Model(ctx, usage.Scope{BusinessLine: "bl", EndUser: "u"}, "late", "a@1", "m", &v1.Usage{InputTokens: 7}, now)
	rows, err := store.Report(ctx, usage.Query{BusinessLine: "bl", From: now.Add(-time.Hour), To: now.Add(time.Hour), GroupBy: usage.ByEndUser})
	if err != nil || len(rows) != 1 || rows[0].Key == "u" || rows[0].InputTokens != 7 {
		t.Fatalf("anonymized usage = %+v, %v", rows, err)
	}
	meter.Store = uncertainUsageRecord{Store: store}
	meter.Model(ctx, usage.Scope{BusinessLine: "bl", EndUser: "u"}, "uncertain", "a@1", "m", &v1.Usage{InputTokens: 3}, now)
	rows, err = store.Report(ctx, usage.Query{BusinessLine: "bl", From: now.Add(-time.Hour), To: now.Add(time.Hour), GroupBy: usage.ByEndUser})
	if err != nil || len(rows) != 1 || rows[0].Key != usage.Anonymous || rows[0].InputTokens != 10 {
		t.Fatalf("uncertain write retained personal data: %+v, %v", rows, err)
	}
}

type uncertainUsageRecord struct{ usage.Store }

func (s uncertainUsageRecord) Record(ctx context.Context, e *usage.Entry) error {
	if err := s.Store.Record(ctx, e); err != nil {
		return err
	}
	return errors.New("record applied but acknowledgement lost")
}
