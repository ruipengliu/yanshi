package pgstore_test

import (
	"context"
	"os"
	"testing"

	"persona/domain"
	"persona/pg/pgtest"
	"persona/store"
	"persona/store/pgstore"
	"persona/store/storetest"
)

func TestPartialReads(t *testing.T) {
	if os.Getenv("YANSHI_TEST_PG") == "" {
		t.Skip("YANSHI_TEST_PG is not set")
	}
	storetest.PartialReads(t, pgstore.New(pgtest.New(t), captureClock), true)
}

// 物化行不是当前叠加版本时，按需读取退回整体装载；一次空的 Update 按当前规则重建后恢复按需读取。
func TestStaleFoldVersion(t *testing.T) {
	if os.Getenv("YANSHI_TEST_PG") == "" {
		t.Skip("YANSHI_TEST_PG is not set")
	}
	ctx := context.Background()
	pool := pgtest.New(t)
	s := pgstore.New(pool, captureClock)
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		b.Seed(domain.SeedData{Commitments: []domain.SeedCommitment{{ID: "C-1", Kind: "mine", Content: "交样板"}}}, captureClock())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE book_meta SET payload = payload - 'FoldVersion'`); err != nil {
		t.Fatal(err)
	}
	stale, err := s.StaleOwners(ctx)
	if err != nil || len(stale) != 1 || stale[0] != (store.Owner{BusinessLine: "bl", EndUser: "u"}) {
		t.Fatalf("stale owners = %v, %v", stale, err)
	}
	plan := domain.ToolReadPlan("list_commitments", "day", []byte(`{}`))
	partial := func() bool {
		var got bool
		if err := s.ViewPlanned(ctx, "bl", "u", plan, func(b *domain.Book) error {
			got = b.Partial()
			if open, err := b.QueryCommitments(domain.CommitmentFilter{}); err != nil || len(open) != 1 {
				t.Fatalf("open commitments = %v, %v", open, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if partial() {
		t.Fatal("stale materialized rows were trusted")
	}
	if err := s.Update(ctx, "bl", "u", func(*domain.Book) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if stale, err := s.StaleOwners(ctx); err != nil || len(stale) != 0 {
		t.Fatalf("rebuilt owner still stale: %v, %v", stale, err)
	}
	if !partial() {
		t.Fatal("rebuilt owner did not use partial reads")
	}
}
