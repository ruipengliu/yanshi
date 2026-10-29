package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"persona/domain"
	"persona/store"
	"xinli/account"
	"xinli/task"
)

func TestAccountBlocksScheduleAndFanout(t *testing.T) {
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
			books, jobs, _ := workStores(t, backend, func() time.Time { return now })
			accounts := jobs.(account.Store)
			owner := task.Owner{BusinessLine: "bl", EndUser: "u"}
			if _, err := accounts.Register(ctx, "identity", account.Account{Owner: owner, Status: account.Active, TimeZone: "UTC", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			service := &account.Service{Store: accounts, Now: func() time.Time { return now }}
			if err := books.Update(ctx, "bl", "u", func(b *domain.Book) error { return collectionFixture(b, "day", "p", now) }); err != nil {
				t.Fatal(err)
			}
			pending, err := books.PendingDomainChanges(ctx, "bl", "u")
			if err != nil || len(pending) == 0 {
				t.Fatal("missing pending facts", err)
			}
			if err := service.Disable(ctx, owner); err != nil {
				t.Fatal(err)
			}
			kernel := &workKernel{}
			o := New(ctx, books, kernel, Options{Tasks: jobs, Accounts: service, Now: func() time.Time { return now }})
			defer finish(t, o)
			if err := o.receiveChanges(ctx, "bl", "u"); !errors.Is(err, account.ErrInactive) {
				t.Fatal("fanout bypassed account", err)
			}
			if err := o.scheduleOwner(ctx, store.Owner{BusinessLine: "bl", EndUser: "u"}); !errors.Is(err, account.ErrInactive) {
				t.Fatal("scheduler bypassed account", err)
			}
			if err := o.processDeletions(ctx, "bl", "u"); !errors.Is(err, account.ErrInactive) {
				t.Fatal("deletion task bypassed account", err)
			}
			after, err := books.PendingDomainChanges(ctx, "bl", "u")
			if err != nil || len(after) != len(pending) {
				t.Fatal("disabled fanout acknowledged facts", err)
			}
			due, err := jobs.Due(ctx, now.Add(time.Hour), 100)
			if err != nil || len(due) != 0 || len(kernel.inputs) != 0 {
				t.Fatal("disabled owner produced work", err)
			}
		})
	}
}
