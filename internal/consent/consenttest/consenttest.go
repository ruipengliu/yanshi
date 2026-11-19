// Package consenttest 是 consent.Store 的一致性套件。
package consenttest

import (
	"context"
	"testing"
	"time"

	"yanshi/internal/consent"
)

func Run(t *testing.T, fresh func(*testing.T) consent.Store) {
	t.Helper()
	t.Run("grant_withdraw_and_isolation", func(t *testing.T) {
		s := fresh(t)
		ctx := context.Background()
		now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
		get := func(bl, eu string, kind consent.Kind, want time.Time) {
			t.Helper()
			if got, err := s.Get(ctx, bl, eu, kind); err != nil || !got.Equal(want) {
				t.Fatalf("Get(%s, %s, %s) = %v, %v; want %v", bl, eu, kind, got, err, want)
			}
		}
		get("bl", "u", consent.Health, time.Time{})
		for _, k := range [][2]string{{"bl", "u"}, {"other", "u"}, {"bl", "other"}} {
			if err := s.Put(ctx, k[0], k[1], consent.Health, now); err != nil {
				t.Fatal(err)
			}
		}
		// 重复登记（重试、用户再次点同意）保留最初的同意时间。
		for range 2 {
			if err := s.Put(ctx, "bl", "u", consent.Health, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		get("bl", "u", consent.Health, now)
		// 类别相互独立：未登记的类别没有记录。
		get("bl", "u", consent.Kind("future"), time.Time{})
		for range 2 {
			if err := s.Delete(ctx, "bl", "u", consent.Health); err != nil {
				t.Fatal(err)
			}
		}
		get("bl", "u", consent.Health, time.Time{})
		get("other", "u", consent.Health, now)
		get("bl", "other", consent.Health, now)
		// 撤回后重新同意是新的同意时间。
		if err := s.Put(ctx, "bl", "u", consent.Health, now.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		get("bl", "u", consent.Health, now.Add(2*time.Hour))
	})
	t.Run("delete_end_user", func(t *testing.T) {
		s := fresh(t)
		ctx := context.Background()
		now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
		for _, k := range []struct {
			bl, eu string
			kind   consent.Kind
		}{{"bl", "u", consent.Health}, {"bl", "u", "future"}, {"other", "u", consent.Health}, {"bl", "v", consent.Health}} {
			if err := s.Put(ctx, k.bl, k.eu, k.kind, now); err != nil {
				t.Fatal(err)
			}
		}
		for range 2 {
			if err := s.DeleteEndUser(ctx, "bl", "u"); err != nil {
				t.Fatal(err)
			}
		}
		for _, k := range []struct {
			bl, eu string
			kind   consent.Kind
			kept   bool
		}{{"bl", "u", consent.Health, false}, {"bl", "u", "future", false}, {"other", "u", consent.Health, true}, {"bl", "v", consent.Health, true}} {
			got, err := s.Get(ctx, k.bl, k.eu, k.kind)
			if err != nil || got.IsZero() == k.kept {
				t.Fatalf("after DeleteEndUser, Get(%s, %s, %s) = %v, %v; kept %v", k.bl, k.eu, k.kind, got, err, k.kept)
			}
		}
	})
}
