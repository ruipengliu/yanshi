package pgstore_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"persona/domain"
	"persona/pg"
	"persona/pg/pgtest"
	"persona/store/pgstore"
)

// New 也可能接管调用方构造的池；Store 自身必须限制借连接，不能只靠 Open 的建连配置。
func TestOperationsBoundPoolAcquire(t *testing.T) {
	for _, operation := range []string{"view", "update", "scan"} {
		t.Run(operation, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			stop, done := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(done)
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					go func() { <-stop; _ = conn.Close() }()
				}
			}()
			var pool *pgxpool.Pool
			defer func() {
				close(stop)
				_ = listener.Close()
				<-done
				if pool != nil {
					pool.Close()
				}
			}()
			cfg, err := pgxpool.ParseConfig("postgres://test@" + listener.Addr().String() + "/test?sslmode=disable")
			if err != nil {
				t.Fatal(err)
			}
			cfg.MaxConns = 1
			cfg.ConnConfig.ConnectTimeout = 10 * time.Second
			pool, err = pgxpool.NewWithConfig(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			s := pgstore.New(pool)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			entered := false
			fn := func(*domain.Book) error { entered = true; return nil }
			started := time.Now()
			switch operation {
			case "view":
				err = s.View(ctx, "bl", "u", fn)
			case "update":
				err = s.Update(ctx, "bl", "u", fn)
			case "scan":
				_, err = s.SchedulingOwners(ctx)
			}
			if !errors.Is(err, context.DeadlineExceeded) || entered || time.Since(started) >= 2500*time.Millisecond {
				t.Fatalf("operation=%s entered=%t err=%v elapsed=%s", operation, entered, err, time.Since(started))
			}
		})
	}
}

// 调用方显式给出更短期限时仍须遵守；超时不进入回调、不改已提交账本。
func TestUpdateBoundsOwnerRowLock(t *testing.T) {
	pool := pgtest.New(t)
	s := pgstore.New(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Update(ctx, "bl", "u", func(book *domain.Book) error {
		book.Seed(domain.SeedData{Projects: []domain.Project{{ID: "P-1", Name: "confirmed"}}}, time.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Rollback(blocker)
	var locked []byte
	if err := blocker.QueryRow(ctx, `SELECT business_line FROM owners WHERE business_line=$1 AND end_user=$2 FOR UPDATE`, []byte("bl"), []byte("u")).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	entered := false
	started := time.Now()
	limited, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	defer stop()
	err = s.Update(limited, "bl", "u", func(*domain.Book) error { entered = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || entered || time.Since(started) >= 2500*time.Millisecond {
		t.Fatalf("entered=%t err=%v elapsed=%s", entered, err, time.Since(started))
	}
	pg.Rollback(blocker)
	if err := s.View(ctx, "bl", "u", func(book *domain.Book) error {
		if len(book.State().Projects) != 1 || book.State().Projects[0].Name != "confirmed" {
			return errors.New("confirmed book changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// 两次正常写入在同一 owner 上排队；每次回调超过旧的两秒预算，仍须依次提交。
func TestSlowOwnerUpdatesQueue(t *testing.T) {
	pool := pgtest.New(t)
	s := pgstore.New(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	entered := make(chan struct{})
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() {
		first <- s.Update(ctx, "bl", "u", func(book *domain.Book) error {
			close(entered)
			select {
			case <-time.After(2300 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
			book.Seed(domain.SeedData{Projects: []domain.Project{{ID: "P-1", Name: "first"}}}, time.Now())
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-first:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() {
		second <- s.Update(ctx, "bl", "u", func(book *domain.Book) error {
			if len(book.State().Projects) != 1 || book.State().Projects[0].Name != "first" {
				return errors.New("queued write missed prior commit")
			}
			select {
			case <-time.After(2300 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		})
	}()
	for _, done := range []<-chan error{first, second} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if err := s.View(ctx, "bl", "u", func(*domain.Book) error {
		select {
		case <-time.After(2300 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}); err != nil {
		t.Fatal(err)
	}
}
