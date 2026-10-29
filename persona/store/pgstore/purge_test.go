package pgstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"persona/domain"
	"persona/pg/pgtest"
	"persona/store"
)

func TestPurgeRestartAndBound(t *testing.T) {
	pool := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	s := New(pool, func() time.Time { return now })
	entered, release := make(chan struct{}), make(chan struct{}, 1)
	defer close(release)
	writer := make(chan error, 1)
	go func() {
		writer <- s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			b.Seed(domain.SeedData{Commitments: []domain.SeedCommitment{{Content: "待删", Kind: "mine"}}}, now)
			if err := b.BindDay("2026-10-27", "day"); err != nil {
				return err
			}
			call := domain.NewCall(b, "week", []byte(`{}`), nil, now)
			call.FileWeek()
			if err := call.Err(); err != nil {
				return err
			}
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err := b.AppendCaptures("2026-10-27", "batch", []domain.CaptureSegment{{Speaker: "wearer", Text: "甲"}, {Speaker: "wearer", Text: "乙"}, {Speaker: "wearer", Text: "丙"}})
			return err
		})
	}()
	select {
	case <-entered:
	case err := <-writer:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	barrier := make(chan error, 1)
	go func() {
		_, err := s.BeginOwnerDeletion(ctx, "bl", "u", now)
		barrier <- err
	}()
	select {
	case err := <-barrier:
		t.Fatal("barrier passed first uncommitted append", err)
	case <-time.After(30 * time.Millisecond):
	}
	release <- struct{}{}
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
	if err := <-barrier; err != nil {
		t.Fatal(err)
	}
	// 直接按数据库中全部业务表计数，后续新增表若漏接清理，本断言也会失败。
	rows, err := pool.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname=current_schema() AND tablename NOT IN ('owners','owner_deletions','persona_schema_migrations') ORDER BY tablename`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	count := func() int {
		t.Helper()
		total := 0
		for _, table := range tables {
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE business_line=$1 AND end_user=$2`, []byte("bl"), []byte("u")).Scan(&n); err != nil {
				t.Fatal(err)
			}
			total += n
		}
		return total
	}
	// 原文删除之后注入账本删除失败，整个批次必须回滚，发现记录仍可供重启恢复。
	beforeFailure := count()
	if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_purge() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected purge failure'; END $$;
		CREATE TRIGGER reject_purge BEFORE DELETE ON book_meta FOR EACH ROW EXECUTE FUNCTION reject_purge()`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Purge(ctx, "bl", "u", 1000, now); err == nil || count() != beforeFailure {
		t.Fatal("failed purge did not roll back", err)
	}
	if pending, err := s.PendingPurges(ctx); err != nil || len(pending) != 1 {
		t.Fatal("failed purge lost discovery", pending, err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER reject_purge ON book_meta; DROP FUNCTION reject_purge()`); err != nil {
		t.Fatal(err)
	}
	before := count()
	if out, err := s.Purge(ctx, "bl", "u", 1, now); err != nil || out.Status != "pending" || count() != before-1 {
		t.Fatal("unbounded batch", out, err)
	}
	// 每一批后关闭并重开独立连接池，覆盖响应丢失、清理者重启及任意中间状态。
	for range 100 {
		reopened, err := pgxpool.NewWithConfig(ctx, pool.Config())
		if err != nil {
			t.Fatal(err)
		}
		s = New(reopened)
		before = count()
		out, err := s.Purge(ctx, "bl", "u", 1, now.Add(time.Hour))
		s.Close()
		if err != nil {
			t.Fatal(err)
		}
		if delta := before - count(); delta < 0 || delta > 1 {
			t.Fatal("purge exceeded budget", delta)
		}
		if out.Status == "completed" {
			break
		}
	}
	s = New(pool)
	out, err := s.OwnerDeletion(ctx, "bl", "u")
	if err != nil || out.Status != "completed" || count() != 0 {
		t.Fatal("purge left physical data", out, err)
	}
	if err := s.Update(ctx, "bl", "u", func(*domain.Book) error {
		t.Error("late write ran")
		return nil
	}); !errors.Is(err, store.ErrOwnerDeleting) {
		t.Fatal(err)
	}
	var owners int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM owners`).Scan(&owners); err != nil || owners != 1 {
		t.Fatal("owner lock row removed", owners, err)
	}
}
