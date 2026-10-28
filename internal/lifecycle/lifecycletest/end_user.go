package lifecycletest

import (
	"context"
	"errors"
	"testing"
	"time"

	"yanshi/internal/lifecycle"
)

func runEndUserDeletions(t *testing.T, newDeletions func(*testing.T) lifecycle.Deletions) {
	runEndUserPrune(t, newDeletions)
	t.Run("EndUserBarrier", func(t *testing.T) {
		ctx := context.Background()
		d := newDeletions(t)
		if _, err := d.GetEndUser(ctx, "bl", "u"); !errors.Is(err, lifecycle.ErrNotFound) {
			t.Fatalf("missing barrier = %v", err)
		}
		mark := &lifecycle.EndUserTombstone{BusinessLine: "bl", EndUser: "u", RequestID: "r", RequestedAt: t0}
		b, err := d.MarkEndUser(ctx, mark)
		if err != nil || b.RequestID != "r" || !b.CompletedAt.IsZero() {
			t.Fatalf("mark = %+v, %v", b, err)
		}
		mark.RequestID = "retry"
		b, err = d.MarkEndUser(ctx, mark)
		if err != nil || b.RequestID != "r" {
			t.Fatalf("retry = %+v, %v", b, err)
		}
		if gone, err := lifecycle.EndUserDeleted(ctx, d, "other", "u"); err != nil || gone {
			t.Fatalf("business line isolation = %v, %v", gone, err)
		}
		if _, err := d.MarkEndUser(ctx, &lifecycle.EndUserTombstone{BusinessLine: "other", EndUser: "u", RequestID: "other-r", RequestedAt: t0}); err != nil {
			t.Fatal(err)
		}
		if gone, err := lifecycle.EndUserDeleted(ctx, d, "bl", "other-u"); err != nil || gone {
			t.Fatalf("EndUser isolation = %v, %v", gone, err)
		}
		if gone, err := lifecycle.EndUserDeleted(ctx, d, "bl", "u"); err != nil || !gone {
			t.Fatalf("barrier = %v, %v", gone, err)
		}
		if err := d.CompleteEndUser(ctx, b, t0, t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := d.PruneEndUsers(ctx, t0.Add(72*time.Hour), 10, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := d.GetEndUser(ctx, "bl", "u"); err != nil {
			t.Fatalf("unfinished barrier pruned: %v", err)
		}
		if err := d.AddEndUserSessions(ctx, b, []string{"s1", "s1"}); err != nil {
			t.Fatal(err)
		}
		if err := d.SealEndUser(ctx, b); err != nil {
			t.Fatal(err)
		}
		pending, err := d.PendingEndUsers(ctx, 10)
		if err != nil || len(pending) != 1 || len(pending[0].Sessions) != 1 || pending[0].Sessions[0] != "s1" {
			t.Fatalf("pending = %+v, %v", pending, err)
		}
		b = pending[0]
		// 读出待完成记录之后又有撤回，旧快照不能提前开启宽限期。
		if err := d.AddEndUserSessions(ctx, b, []string{"s2"}); err != nil {
			t.Fatal(err)
		}
		done := t0.Add(time.Hour)
		expires := done.Add(24 * time.Hour)
		if err := d.CompleteEndUser(ctx, b, done, expires); err != nil {
			t.Fatal(err)
		}
		b, err = d.GetEndUser(ctx, "bl", "u")
		if err != nil || !b.CompletedAt.IsZero() {
			t.Fatalf("stale completion = %+v, %v", b, err)
		}
		if err := d.CompleteEndUser(ctx, b, done, expires); err != nil {
			t.Fatal(err)
		}
		if err := d.CompleteEndUser(ctx, b, expires, expires.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		b, err = d.GetEndUser(ctx, "bl", "u")
		if err != nil || !b.CompletedAt.Equal(done) || !b.ExpiresAt.Equal(expires) {
			t.Fatalf("completion = %+v, %v", b, err)
		}
		if err := d.AddEndUserSessions(ctx, b, []string{"s2", "s1"}); err != nil {
			t.Fatal(err)
		}
		b, err = d.MarkEndUser(ctx, mark)
		if err != nil || !b.ExpiresAt.Equal(expires) {
			t.Fatalf("retry changed grace = %+v, %v", b, err)
		}
		if pending, err := d.PendingEndUsers(ctx, 10); err != nil || len(pending) != 0 {
			t.Fatalf("completed barrier pending = %+v, %v", pending, err)
		}
		if err := d.PruneEndUsers(ctx, expires.Add(-time.Nanosecond), 10, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := d.GetEndUser(ctx, "bl", "u"); err != nil {
			t.Fatalf("grace period barrier missing: %v", err)
		}
		if err := d.PruneEndUsers(ctx, expires, 10, nil); err != nil {
			t.Fatal(err)
		}
		if gone, err := lifecycle.EndUserDeleted(ctx, d, "bl", "u"); err != nil || gone {
			t.Fatalf("expired barrier = %v, %v", gone, err)
		}
		if gone, err := lifecycle.EndUserDeleted(ctx, d, "other", "u"); err != nil || !gone {
			t.Fatalf("unfinished business line pruned = %v, %v", gone, err)
		}
		b, err = d.MarkEndUser(ctx, mark)
		if err != nil || b.RequestID != "retry" {
			t.Fatalf("reused EndUser = %+v, %v", b, err)
		}
	})
}

func runEndUserPrune(t *testing.T, newDeletions func(*testing.T) lifecycle.Deletions) {
	t.Run("EndUserPruneCleanup", func(t *testing.T) {
		ctx := context.Background()
		d := newDeletions(t)
		expires := t0.Add(time.Hour)
		for _, bl := range []string{"a", "b", "unfinished", "future"} {
			b, err := d.MarkEndUser(ctx, &lifecycle.EndUserTombstone{BusinessLine: bl, EndUser: "u", RequestID: bl, RequestedAt: t0})
			if err != nil {
				t.Fatal(err)
			}
			if bl == "unfinished" {
				continue
			}
			if err := d.SealEndUser(ctx, b); err != nil {
				t.Fatal(err)
			}
			until := expires
			if bl == "future" {
				until = until.Add(time.Hour)
			}
			if err := d.CompleteEndUser(ctx, b, t0, until); err != nil {
				t.Fatal(err)
			}
		}
		failed := errors.New("cleanup unavailable")
		var calls []string
		cleanup := func(_ context.Context, bl, eu string) error {
			if eu != "u" {
				t.Fatalf("cleanup used wrong EndUser %q", eu)
			}
			calls = append(calls, bl)
			if bl == "a" {
				return failed
			}
			return nil
		}
		if err := d.PruneEndUsers(ctx, expires.Add(-time.Nanosecond), 10, cleanup); err != nil || len(calls) != 0 {
			t.Fatalf("cleanup before expiry = %v, %v", calls, err)
		}
		if err := d.PruneEndUsers(ctx, expires, 1, cleanup); !errors.Is(err, failed) || len(calls) != 1 || calls[0] != "a" {
			t.Fatalf("limited cleanup = %v, %v", calls, err)
		}
		calls = nil
		if err := d.PruneEndUsers(ctx, expires, 10, cleanup); !errors.Is(err, failed) || len(calls) != 2 || calls[0] != "a" || calls[1] != "b" {
			t.Fatalf("partial cleanup = %v, %v", calls, err)
		}
		if _, err := d.GetEndUser(ctx, "b", "u"); !errors.Is(err, lifecycle.ErrNotFound) {
			t.Fatalf("successful cleanup retained barrier: %v", err)
		}
		b, err := d.GetEndUser(ctx, "a", "u")
		if err != nil || !b.ExpiresAt.Equal(expires) {
			t.Fatalf("failed cleanup lost barrier or changed grace: %+v, %v", b, err)
		}
		calls = nil
		if err := d.PruneEndUsers(ctx, expires, 10, func(_ context.Context, bl, eu string) error {
			calls = append(calls, bl+"/"+eu)
			return nil
		}); err != nil || len(calls) != 1 || calls[0] != "a/u" {
			t.Fatalf("retry = %v, %v", calls, err)
		}
		if _, err := d.GetEndUser(ctx, "a", "u"); !errors.Is(err, lifecycle.ErrNotFound) {
			t.Fatalf("retry retained barrier: %v", err)
		}
		for _, bl := range []string{"unfinished", "future"} {
			if _, err := d.GetEndUser(ctx, bl, "u"); err != nil {
				t.Fatalf("pruned ineligible barrier %s: %v", bl, err)
			}
		}
	})
}
