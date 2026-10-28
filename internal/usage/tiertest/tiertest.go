// Package tiertest 是 TierStore 的一致性套件。
package tiertest

import (
	"context"
	"testing"
	"time"

	"yanshi/internal/usage"
)

func Run(t *testing.T, fresh func(*testing.T) usage.TierStore) {
	t.Helper()
	t.Run("assignment_and_isolation", func(t *testing.T) {
		s := fresh(t)
		ctx := context.Background()
		now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
		get := func(bl, eu, want string) {
			t.Helper()
			if got, err := s.Get(ctx, bl, eu); err != nil || got != want {
				t.Fatalf("Get(%s, %s) = %q, %v; want %q", bl, eu, got, err, want)
			}
		}
		get("bl", "u", "")
		for _, key := range [][2]string{{"bl", "u"}, {"other", "u"}, {"bl", "other"}} {
			if err := s.Put(ctx, key[0], key[1], "pro", now); err != nil {
				t.Fatal(err)
			}
		}
		for range 2 {
			if err := s.Put(ctx, "bl", "u", "plus", now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
		}
		get("bl", "u", "plus")
		get("other", "u", "pro")
		get("bl", "other", "pro")
		for range 2 {
			if err := s.Delete(ctx, "bl", "u"); err != nil {
				t.Fatal(err)
			}
		}
		get("bl", "u", "")
		get("other", "u", "pro")
		get("bl", "other", "pro")
		if err := s.Put(ctx, "bl", "u", "free", now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		get("bl", "u", "free")
	})
}
