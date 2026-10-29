package pgusage_test

import (
	"context"
	"testing"
	"time"

	"yanshi/internal/ids"
	"yanshi/internal/pg"
	"yanshi/internal/pg/pgtest"
	"yanshi/internal/usage"
	"yanshi/internal/usage/pgusage"
)

func TestUsageMigrationPreservesLegacyAndAnonymousKeys(t *testing.T) {
	ctx := context.Background()
	pool, _ := pgtest.Fresh(t)
	s := pgusage.Store{Pool: pool}
	at := time.Unix(1000, 0).UTC()
	for _, id := range []string{"use_old", "call_event/1"} {
		if err := s.Record(ctx, &usage.Entry{ID: id, BusinessLine: "bl", EndUser: "u", Kind: usage.Call, Cost: 10, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AnonymizeEntry(ctx, "u", "call_event/1"); err != nil {
		t.Fatal(err)
	}
	// 恢复旧表形态再执行新增迁移，确保匿名历史也从原 ID 得到同一幂等键。
	if _, err := pool.Exec(ctx, `ALTER TABLE usage_entries DROP COLUMN idempotency_key;
		DELETE FROM schema_migrations WHERE version IN (39, 40)`); err != nil {
		t.Fatal(err)
	}
	if err := pg.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{"use_old", "call_event/1"} {
		id, err := ids.WithEndUser("use", "encoded-"+map[string]string{"use_old": "model", "call_event/1": "call"}[old], "u")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Record(ctx, &usage.Entry{ID: id, LegacyID: old, BusinessLine: "bl", EndUser: "u", Kind: usage.Call, Cost: 10, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	var count, anonymous, unchanged int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE end_user = '~'), count(*) FILTER (WHERE id = idempotency_key) FROM usage_entries`).Scan(&count, &anonymous, &unchanged); err != nil {
		t.Fatal(err)
	}
	if count != 2 || anonymous != 1 || unchanged != 2 {
		t.Fatalf("migration/retry changed old rows: total=%d anonymous=%d unchanged=%d", count, anonymous, unchanged)
	}
	if got, err := s.Spent(ctx, "bl", "", at.Truncate(time.Hour), at.Truncate(time.Hour).Add(time.Hour)); err != nil || got != 20 {
		t.Fatalf("migration/retry changed aggregates: %d, %v", got, err)
	}
}
