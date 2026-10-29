package pgstore

import (
	"context"
	"strings"
	"testing"
	"time"

	"persona/domain"
	"persona/pg/pgtest"
)

func TestDomainChangeWriteFailureRollsBackFact(t *testing.T) {
	pool := pgtest.New(t)
	s := New(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_change() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected change failure'; END $$;
		CREATE TRIGGER reject_change BEFORE INSERT ON domain_changes FOR EACH ROW EXECUTE FUNCTION reject_change()`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	file := func(b *domain.Book) error {
		c := domain.NewCall(b, "day", []byte(`{"day":{"headline":"事务"}}`), nil, now)
		c.FileDay()
		return c.Err()
	}
	if err := s.Update(ctx, "bl", "u", file); err == nil {
		t.Fatal("injected insert failure ignored")
	}
	for _, table := range []string{"days", "accepted_facts", "domain_changes"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE business_line=$1 AND end_user=$2`, []byte("bl"), []byte("u")).Scan(&n); err != nil || n != 0 {
			t.Fatal("partially committed fact", table, n, err)
		}
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER reject_change ON domain_changes; DROP FUNCTION reject_change()`); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, "bl", "u", file); err != nil {
		t.Fatal(err)
	}
	changes, err := s.PendingDomainChanges(ctx, "bl", "u")
	if err != nil || len(changes) != 1 || changes[0].Version != 1 {
		t.Fatal("rolled back version consumed", changes, err)
	}
	id := changes[0].ID
	if err := s.AckDomainChanges(ctx, "bl", "u", id, 1, now); err != nil {
		t.Fatal(err)
	}
	// 重复确认不移动保留起点，恰满七天才可物理清理。
	if err := s.AckDomainChanges(ctx, "bl", "u", id, 1, now.Add(domain.DomainChangeRetention-time.Second)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM domain_changes WHERE status='acknowledged'`).Scan(&n); err != nil || n != 1 {
		t.Fatal("acknowledged history lost before retention", n, err)
	}
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		b.PruneDomainChanges(now.Add(domain.DomainChangeRetention))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM domain_changes`).Scan(&n); err != nil || n != 0 {
		t.Fatal("confirmed history retained past explicit cleanup", n, err)
	}
}

func TestDomainChangeDiscoveryUsesPendingIndex(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	// 小测试表的代价估计通常选顺序扫描；禁用它以证明谓词能使用部分索引。
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan=off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `EXPLAIN SELECT DISTINCT business_line,end_user FROM domain_changes t WHERE status='pending' AND NOT EXISTS (SELECT 1 FROM owner_deletions d WHERE d.business_line=t.business_line AND d.end_user=t.end_user)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line)
	}
	if err := rows.Err(); err != nil || !strings.Contains(plan.String(), "domain_changes_pending") {
		t.Fatal("pending owner query lacks partial index", plan.String(), err)
	}
}
