package pgstore_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"persona/domain"
	"persona/pg/pgtest"
	"persona/store"
	"persona/store/pgstore"
	"persona/store/storetest"
)

const date = "2026-10-27"

func TestStore(t *testing.T) {
	// 套件 factory 没有子测试的 T，跳过须在进入子测试之前完成。
	if os.Getenv("YANSHI_TEST_PG") == "" {
		t.Skip("YANSHI_TEST_PG is not set")
	}
	storetest.Run(t, func() store.Store { return pgstore.New(pgtest.New(t), captureClock) })
}

func TestOwnerLocks(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing_%t", existing), func(t *testing.T) {
			pool := pgtest.New(t)
			s1 := pgstore.New(pool, captureClock)
			other, err := pgxpool.NewWithConfig(context.Background(), pool.Config())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(other.Close)
			s2 := pgstore.New(other, captureClock)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if existing {
				if err := s1.Update(ctx, "bl", "u", func(*domain.Book) error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			entered, release := make(chan struct{}), make(chan struct{}, 1)
			first := make(chan error, 1)
			go func() {
				first <- s1.Update(ctx, "bl", "u", func(b *domain.Book) error {
					b.Seed(domain.SeedData{Projects: []domain.Project{{ID: "P-1"}}}, time.Time{})
					close(entered)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			defer close(release)
			select {
			case <-entered:
			case err := <-first:
				t.Fatal("first update failed", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			waiting, stop := context.WithTimeout(ctx, 200*time.Millisecond)
			err = s2.Update(waiting, "bl", "u", func(*domain.Book) error {
				return errors.New("same owner entered while locked")
			})
			stop()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("same owner did not wait for lock: %v", err)
			}
			// 第一条连接仍持锁时，第二个 owner 必须能完成提交。
			independent, stop := context.WithTimeout(ctx, 2*time.Second)
			err = s2.Update(independent, "bl", "other", func(b *domain.Book) error {
				_, err := b.AppendCaptures(date, "one", []domain.CaptureSegment{{Speaker: "wearer", Text: "独立"}})
				return err
			})
			stop()
			if err != nil {
				t.Fatal("different owner blocked", err)
			}
			// 用通知而非关闭释放，defer 仍能覆盖提前失败。
			release <- struct{}{}
			if err := <-first; err != nil {
				t.Fatal(err)
			}
			if err := s2.Update(ctx, "bl", "u", func(b *domain.Book) error {
				if len(b.State().Projects) != 1 {
					return errors.New("lock successor missed committed book")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRollbackAndLifetime(t *testing.T) {
	for _, mode := range []string{"error", "panic", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s := pgstore.New(pgtest.New(t), captureClock)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("rollback")
			var access domain.CaptureAccess
			var scan iter.Seq2[domain.CaptureRecord, error]
			var result error
			func() {
				defer func() {
					r := recover()
					if mode == "panic" && r != failure || mode != "panic" && r != nil {
						t.Errorf("unexpected panic: %v", r)
					}
				}()
				result = s.Update(ctx, "bl", "u", func(b *domain.Book) error {
					access = b.CaptureAccess()
					b.Seed(domain.SeedData{Projects: []domain.Project{{ID: "P-1"}}}, time.Time{})
					if _, err := b.AppendCaptures(date, "one", []domain.CaptureSegment{{Speaker: "wearer", Text: "撤回"}}); err != nil {
						return err
					}
					scan = access.Scan(date, domain.Ref{}, domain.Ref{})
					switch mode {
					case "panic":
						panic(failure)
					case "cancel":
						cancel()
						return nil
					default:
						return failure
					}
				})
			}()
			if mode == "error" && !errors.Is(result, failure) || mode == "cancel" && !errors.Is(result, context.Canceled) {
				t.Fatal(result)
			}
			if access == nil || scan == nil {
				t.Fatal("callback did not reach capture write", result)
			}
			if _, err := access.Batch(date, 1); !errors.Is(err, domain.ErrCaptureTransactionClosed) {
				t.Fatal("escaped capture access", err)
			}
			for _, err := range scan {
				if !errors.Is(err, domain.ErrCaptureTransactionClosed) {
					t.Fatal("escaped scan", err)
				}
			}
			if err := s.Update(context.Background(), "bl", "u", func(b *domain.Book) error {
				if len(b.State().Projects) != 0 {
					return errors.New("ledger survived rollback")
				}
				if _, found, err := b.CaptureAccess().Lookup(date, domain.Ref{Seq: 1}); err != nil || found {
					return fmt.Errorf("capture survived rollback: %t %v", found, err)
				}
				refs, err := b.AppendCaptures(date, "one", []domain.CaptureSegment{{Speaker: "wearer", Text: "重试"}})
				if err == nil && !reflect.DeepEqual(refs, []string{"1.0"}) {
					return errors.New("allocation survived rollback")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCaptureUint64AndScanPages(t *testing.T) {
	pool := pgtest.New(t)
	s := pgstore.New(pool, captureClock)
	ctx := context.Background()
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		segments := make([]domain.CaptureSegment, 260)
		for i := range segments {
			segments[i] = domain.CaptureSegment{Speaker: "wearer", Text: "分页"}
		}
		if err := b.CaptureAccess().Append(date, math.MaxUint64, segments); err != nil {
			return err
		}
		return b.CaptureAccess().Erase(date, domain.Ref{Seq: math.MaxUint64, Index: 127}, domain.Ref{Seq: math.MaxUint64, Index: 129})
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		access := b.CaptureAccess()
		count, previous := 0, -1
		for r, err := range access.Scan(date, domain.Ref{}, domain.Ref{}) {
			if err != nil {
				return err
			}
			if r.Ref.Seq != math.MaxUint64 || r.Ref.Index <= previous || r.Ref.Index >= 127 && r.Ref.Index <= 129 {
				return errors.New("scan order or erased gap differs")
			}
			previous = r.Ref.Index
			count++
		}
		if count != 257 {
			return fmt.Errorf("scan count: %d", count)
		}
		for _, err := range access.Scan(date, domain.Ref{}, domain.Ref{}) {
			if err != nil {
				return err
			}
			break
		}
		// 提前退出后立即用同一个事务连接写入，检验 Rows 没有占住连接。
		if err := access.Append(date, 1, []domain.CaptureSegment{{Speaker: "wearer", Text: "后写"}}); err != nil {
			return err
		}
		batch, err := access.Batch(date, math.MaxUint64)
		if err != nil {
			return err
		}
		if len(batch) != 260 || !batch[128].Equal(domain.CaptureSegment{}) {
			return errors.New("erasure lost batch slots")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM capture_segments WHERE payload IS NULL`).Scan(&rows); err != nil || rows != 3 {
		t.Fatal("erasure did not clear full payload", rows, err)
	}
}

func TestChangesetReplacementKeepsPosition(t *testing.T) {
	s := pgstore.New(pgtest.New(t), captureClock)
	ctx := context.Background()
	for _, seq := range []int{1, 2, 1} {
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			args := []byte(fmt.Sprintf(`{"source":{"from":"#%d.0","to":"#%d.0"}}`, seq, seq))
			call := domain.NewCall(b, "unbound", args, nil, time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC))
			call.FileChanges()
			return call.Err()
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
		changes := b.State().Changesets
		if len(changes) != 2 || changes[0].ID != "CS-0003" || changes[1].ID != "CS-0002" {
			return fmt.Errorf("replacement changed fold position: %+v", changes)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
