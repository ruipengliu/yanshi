package pgstore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"persona/domain"
	"persona/pg/pgtest"
)

func TestViewOverlay(t *testing.T) {
	pool := pgtest.New(t)
	s := New(pool)
	ctx := context.Background()
	const date = "2026-10-27"
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		return b.CaptureAccess().Append(date, 2, []domain.CaptureSegment{{Speaker: "wearer", Text: "中间"}})
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
		a := b.CaptureAccess().(*captureTx)
		var readOnly, isolation string
		if err := a.tx.QueryRow(ctx, `SHOW transaction_read_only`).Scan(&readOnly); err != nil {
			return err
		}
		if err := a.tx.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
			return err
		}
		if readOnly != "on" || isolation != "repeatable read" {
			return fmt.Errorf("incorrect View transaction: %s %s", readOnly, isolation)
		}
		for _, seq := range []uint64{1, 3} {
			if err := a.Append(date, seq, []domain.CaptureSegment{{Speaker: "wearer", Text: "演练"}}); err != nil {
				return err
			}
		}
		seq := uint64(0)
		for r, err := range a.Scan(date, domain.Ref{}, domain.Ref{}) {
			if err != nil {
				return err
			}
			seq++
			if r.Ref.Seq != seq {
				return errors.New("View overlay order differs")
			}
		}
		if seq != 3 {
			return errors.New("View overlay missing append")
		}
		if err := a.Erase(date, domain.Ref{}, domain.Ref{Seq: 2}); err != nil {
			return err
		}
		for _, seq := range []uint64{1, 2} {
			batch, err := a.Batch(date, seq)
			if err != nil {
				return err
			}
			if len(batch) != 1 || batch[0] != (domain.CaptureSegment{}) {
				return errors.New("View erasure retained segment")
			}
			if _, found, err := a.Lookup(date, domain.Ref{Seq: seq}); err != nil || found {
				return fmt.Errorf("View erasure lookup: %t %v", found, err)
			}
		}
		for _, before := range []bool{true, false} {
			r, found, err := a.Adjacent(date, domain.Ref{}, before)
			if err != nil || !found || r.Ref.Seq != 3 {
				return fmt.Errorf("View adjacent: %v %t %v", r.Ref, found, err)
			}
		}
		if err := a.Erase(date, domain.Ref{}, domain.Ref{}); err != nil {
			return err
		}
		// 无界擦除只影响当时已有的片段，不能把后来追加的片段也隐藏。
		if err := a.Append(date, 4, []domain.CaptureSegment{{Speaker: "wearer", Text: "擦除后追加"}}); err != nil {
			return err
		}
		count := 0
		for r, err := range a.Scan(date, domain.Ref{}, domain.Ref{}) {
			if err != nil {
				return err
			}
			if r.Ref.Seq != 4 {
				return errors.New("View erase was not respected")
			}
			count++
		}
		if count != 1 {
			return errors.New("View erased later append")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM capture_segments WHERE payload IS NOT NULL`).Scan(&count); err != nil || count != 1 {
		t.Fatal("View wrote to database", count, err)
	}
}

func TestDatabaseCaptureErrors(t *testing.T) {
	pool := pgtest.New(t)
	s := New(pool)
	ctx := context.Background()
	for _, operation := range []string{"lookup", "adjacent", "batch", "scan"} {
		t.Run(operation, func(t *testing.T) {
			err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
				a := b.CaptureAccess().(*captureTx)
				// 真正使 PG 事务失败，验证驱动错误没有被当作引用不存在。
				if _, err := a.tx.Exec(ctx, `SELECT 1/0`); err == nil {
					return errors.New("fault did not abort transaction")
				}
				var err error
				switch operation {
				case "lookup":
					_, _, err = a.Lookup("2026-10-27", domain.Ref{Seq: 1})
				case "adjacent":
					_, _, err = a.Adjacent("2026-10-27", domain.Ref{}, false)
				case "batch":
					_, err = a.Batch("2026-10-27", 1)
				case "scan":
					for _, readErr := range a.Scan("2026-10-27", domain.Ref{}, domain.Ref{}) {
						err = readErr
						break
					}
				}
				return err
			})
			if err == nil {
				t.Fatal("database read failure was hidden")
			}
		})
	}
}

func TestScanCancellation(t *testing.T) {
	s := New(pgtest.New(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		a := b.CaptureAccess()
		if err := a.Append("2026-10-27", 1, []domain.CaptureSegment{{Text: "一"}, {Text: "二"}}); err != nil {
			return err
		}
		count := 0
		for _, err := range a.Scan("2026-10-27", domain.Ref{}, domain.Ref{}) {
			if err != nil {
				return err
			}
			count++
			if count > 1 {
				return errors.New("cancelled scan yielded another record")
			}
			cancel()
		}
		return errors.New("cancelled scan hid cancellation")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestJSONBNulParity(t *testing.T) {
	s := New(pgtest.New(t))
	ctx := context.Background()
	const text = "前\x00后\\u0000"
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		b.Seed(domain.SeedData{Projects: []domain.Project{{ID: "P-1", Name: text}}}, time.Time{})
		_, err := b.AppendCaptures("2026-10-27", "one", []domain.CaptureSegment{{Speaker: "wearer", Text: text}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
		if b.State().Projects[0].Name != text {
			return errors.New("JSONB changed ledger NUL")
		}
		a := b.CaptureAccess()
		segment, found, err := a.Lookup("2026-10-27", domain.Ref{Seq: 1})
		if err != nil || !found || segment.Text != text {
			return fmt.Errorf("JSONB changed capture NUL: %t %v", found, err)
		}
		for record, err := range a.Scan("2026-10-27", domain.Ref{}, domain.Ref{}) {
			if err != nil {
				return err
			}
			if record.Segment.Text != text {
				return errors.New("scan changed capture NUL")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
