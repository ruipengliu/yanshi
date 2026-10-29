package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"persona/domain"
	"persona/pg/pgtest"
	"persona/store"
	"persona/store/memstore"
	"persona/store/pgstore"
)

func TestProductionWorkQualification(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			now := captureClock()
			var books store.Store = memstore.New(captureClock)
			if backend == "pg" {
				books = pgstore.New(pgtest.New(t), captureClock)
			}
			x := newRuntime(books, Options{Now: captureClock})
			meta := mcp.Meta{"yanshi/business_line": "bl", "yanshi/end_user": "u", "yanshi/session": "orphan"}
			call := func(handler func(*domain.Call) string, read bool, want string, rejected bool) {
				t.Helper()
				result, err := x.call(ctx, meta, []byte(`{}`), handler, read)
				if err != nil {
					t.Fatal(err)
				}
				if result.IsError != rejected || len(result.Content) != 1 || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, want) {
					t.Fatal(result)
				}
			}
			call((*domain.Call).FileWeek, false, domain.ErrWorkCommand.Error(), true)
			lease := domain.WorkLease{Task: "t", Sequence: 1, Generation: 1, Until: now.Add(time.Minute)}
			if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
				expected, err := b.ReserveWork("week/2026-10-22", lease, now)
				if err != nil {
					return err
				}
				if err := b.BindWork("week/2026-10-22", expected, lease, "week", now); err != nil {
					return err
				}
				return b.RegisterWorkCommand("week/2026-10-22", "week", "i", "week", lease, now)
			}); err != nil {
				t.Fatal(err)
			}
			meta["yanshi/session"] = "week"
			// 当前绑定仍有效时，handler 内的命令类型拒收也须带 IsError，不能只修前置检查。
			for _, handler := range []func(*domain.Call) string{(*domain.Call).FileChanges, (*domain.Call).FileDay, (*domain.Call).SetStatus, (*domain.Call).UpdateCommitments} {
				call(handler, false, domain.ErrWorkCommand.Error(), true)
			}
			call((*domain.Call).FileWeek, false, "已收到本周的觉察", false)
			if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
				return b.FinishWorkCommand("week", "i")
			}); err != nil {
				t.Fatal(err)
			}
			call((*domain.Call).FileWeek, false, domain.ErrWorkCommand.Error(), true)
			call((*domain.Call).WeekOverview, true, "", false)
			if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error {
				return b.RetireWork("week/2026-10-22", "week", now)
			}); err != nil {
				t.Fatal(err)
			}
			call((*domain.Call).FileWeek, false, domain.ErrWorkRetired.Error(), true)
			call((*domain.Call).WeekOverview, true, domain.ErrWorkRetired.Error(), true)
		})
	}
}
