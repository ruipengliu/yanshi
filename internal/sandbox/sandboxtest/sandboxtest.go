// Package sandboxtest 是 sandbox.Activity 实现的一致性测试套件。
package sandboxtest

import (
	"context"
	"testing"
	"time"

	"yanshi/internal/sandbox"
)

func RunActivity(t *testing.T, newActivity func(t *testing.T) sandbox.Activity) {
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("ClaimIdleOnlyOldAndOnce", func(t *testing.T) {
		a := newActivity(t)
		_ = a.Touch(ctx, "old", t0)
		_ = a.Touch(ctx, "fresh", t0.Add(time.Hour))
		ids, err := a.ClaimIdle(ctx, t0.Add(time.Minute), 10)
		if err != nil || len(ids) != 1 || ids[0] != "old" {
			t.Fatalf("claim = %v, %v", ids, err)
		}
		if ids, _ := a.ClaimIdle(ctx, t0.Add(time.Minute), 10); len(ids) != 0 {
			t.Fatalf("claimed twice: %v", ids)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		a := newActivity(t)
		_ = a.Touch(ctx, "x", t0)
		if err := a.Delete(ctx, "x"); err != nil {
			t.Fatal(err)
		}
		if ids, _ := a.ClaimIdle(ctx, t0.Add(time.Hour), 10); len(ids) != 0 {
			t.Fatalf("deleted record still claimable: %v", ids)
		}
	})

	t.Run("TouchNeverMovesBackwards", func(t *testing.T) {
		a := newActivity(t)
		_ = a.Touch(ctx, "s", t0.Add(time.Hour))
		_ = a.Touch(ctx, "s", t0)
		if ids, _ := a.ClaimIdle(ctx, t0.Add(time.Minute), 10); len(ids) != 0 {
			t.Fatalf("older touch overrode newer: %v", ids)
		}
	})

	t.Run("Limit", func(t *testing.T) {
		a := newActivity(t)
		for _, id := range []string{"a", "b", "c"} {
			_ = a.Touch(ctx, id, t0)
		}
		ids, _ := a.ClaimIdle(ctx, t0.Add(time.Minute), 2)
		rest, _ := a.ClaimIdle(ctx, t0.Add(time.Minute), 2)
		if len(ids) != 2 || len(rest) != 1 {
			t.Fatalf("claims = %v then %v", ids, rest)
		}
	})
}
