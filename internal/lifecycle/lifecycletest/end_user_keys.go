package lifecycletest

import (
	"context"
	"errors"
	"testing"
	"time"

	"yanshi/internal/lifecycle"
)

func indexEndUserKeys(t *testing.T, x lifecycle.Index) {
	ctx := context.Background()
	at := time.Unix(1000, 0).UTC()
	row := &lifecycle.Session{ID: sid("same"), BusinessLine: "bl", EndUser: "owner", CreatedAt: at, LastInputAt: at}
	if err := x.Put(ctx, row); err != nil {
		t.Fatal(err)
	}
	// Session ID 仍是全局身份；重复 Put 不改变真实归属，错误用户不能据此更新原行。
	other := *row
	other.EndUser = "other"
	if err := x.Put(ctx, &other); err != nil {
		t.Fatal(err)
	}
	if _, err := x.Get(ctx, "other", row.ID); !errors.Is(err, lifecycle.ErrNotFound) {
		t.Fatalf("wrong EndUser read: %v", err)
	}
	for _, update := range []func() error{
		func() error { return x.Touch(ctx, "other", row.ID, at.Add(time.Hour)) },
		func() error { return x.SetNextDue(ctx, "other", row.ID, at.Add(time.Hour)) },
		func() error { return x.MarkClosed(ctx, "other", row.ID, at.Add(time.Hour)) },
		func() error { return x.Delete(ctx, "other", row.ID) },
	} {
		if err := update(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := x.LookupSession(ctx, row.ID)
	if err != nil || *got != *row {
		t.Fatalf("wrong EndUser changed index: %+v, %v", got, err)
	}
	if got, err := x.Get(ctx, got.EndUser, got.ID); err != nil || got.EndUser != "owner" {
		t.Fatalf("real EndUser lookup: %+v, %v", got, err)
	}
	if err := x.Delete(ctx, row.EndUser, row.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := x.LookupSession(ctx, row.ID); !errors.Is(err, lifecycle.ErrNotFound) {
		t.Fatalf("deleted index found: %v", err)
	}
}
