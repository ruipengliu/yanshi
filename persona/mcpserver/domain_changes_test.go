package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"persona/domain"
	"persona/pg/pgtest"
	"persona/store"
	"persona/store/memstore"
	"persona/store/pgstore"
)

// 在 Update 已提交但尚未返回 MCP 入口时退出，通知与回执都没有机会执行。
type exitAfterCommit struct {
	store.Store
	exit error
}

func (s exitAfterCommit) Update(ctx context.Context, bl, eu string, fn func(*domain.Book) error) error {
	if err := s.Store.Update(ctx, bl, eu, fn); err != nil {
		return err
	}
	panic(s.exit)
}

func TestDomainChangesSurviveExitBeforeNotification(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		for _, kind := range []string{domain.FactDay, domain.FactWeek} {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				ctx := context.Background()
				now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
				var s store.Store
				var reopen func() store.Store
				if backend == "postgres" {
					pool := pgtest.New(t)
					config := pool.Config()
					persistent := pgstore.New(pool)
					s = persistent
					reopen = func() store.Store {
						persistent.Close()
						pool, err := pgxpool.NewWithConfig(ctx, config)
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(pool.Close)
						return pgstore.New(pool)
					}
				} else {
					s = memstore.New()
					reopen = func() store.Store {
						// 内存实现本身不抗进程退出；用已提交的账本快照重开验证恢复格式，不能重放工具。
						var data []byte
						if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
							var err error
							data, err = json.Marshal(b)
							return err
						}); err != nil {
							t.Fatal(err)
						}
						restored := memstore.New()
						if err := restored.Update(ctx, "bl", "u", func(b *domain.Book) error { return json.Unmarshal(data, b) }); err != nil {
							t.Fatal(err)
						}
						return restored
					}
				}
				handler := (*domain.Call).FileDay
				if kind == domain.FactWeek {
					handler = (*domain.Call).FileWeek
				}
				meta := mcp.Meta{"yanshi/business_line": "bl", "yanshi/end_user": "u", "yanshi/session": "work"}
				args := []byte(`{"day":{"headline":"当天事务"},"habits":[]}`)
				exit := errors.New("process exited after commit")
				notified := false
				x := newRuntime(exitAfterCommit{s, exit}, Options{AllowUnmanaged: true, Now: func() time.Time { return now }, Notifier: notifierFunc(func(context.Context, string, string, string, string, []string) {
					notified = true
				})})
				func() {
					defer func() {
						if got := recover(); got != exit {
							t.Fatalf("wrong exit: %v", got)
						}
					}()
					_, _ = x.call(ctx, meta, args, handler, false)
					t.Fatal("call returned after process exit")
				}()
				if notified {
					t.Fatal("notification ran before exit")
				}
				s = reopen()
				if owners, err := s.PendingChangeOwners(ctx); err != nil || !reflect.DeepEqual(owners, []store.Owner{{BusinessLine: "bl", EndUser: "u"}}) {
					t.Fatal("restart did not discover owner", owners, err)
				}
				changes, err := s.PendingDomainChanges(ctx, "bl", "u")
				if err != nil || len(changes) != 1 || changes[0].Kind != kind || changes[0].Version != 1 {
					t.Fatal("restart lost committed change", changes, err)
				}
				if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
					fact, ok := b.AcceptedFactByID(changes[0].ID)
					if !ok || fact != changes[0].Fact || len(b.State().Days)+len(b.State().Weeks) != 1 {
						t.Fatal("restart lost accepted fact")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				// 返回丢失后再次交付，重开的入口不得分配新版本。
				x = newRuntime(s, Options{AllowUnmanaged: true, Now: func() time.Time { return now.Add(24 * time.Hour) }})
				if result, err := x.call(ctx, meta, args, handler, false); err != nil || result == nil {
					t.Fatal(result, err)
				}
				if retry, err := s.PendingDomainChanges(ctx, "bl", "u"); err != nil || !reflect.DeepEqual(retry, changes) {
					t.Fatal("retry after restart duplicated acceptance", retry, err)
				}
			})
		}
	}
}
