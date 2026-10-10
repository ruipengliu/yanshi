// Package notifytest 是 notify.Registry 实现的一致性测试套件。
package notifytest

import (
	"context"
	"testing"
	"time"

	"yanshi/internal/notify"
)

var t0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

func dev(id, token string) *notify.Device {
	return &notify.Device{BusinessLine: "bl", EndUser: "u", DeviceID: id, Label: id, Kind: "phone", Platform: "apns", Token: token}
}

func ids(t *testing.T, r notify.Registry, bl, eu string) []string {
	t.Helper()
	ds, err := r.List(context.Background(), bl, eu)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range ds {
		out = append(out, d.DeviceID)
	}
	return out
}

func Run(t *testing.T, newRegistry func(t *testing.T) notify.Registry) {
	ctx := context.Background()

	t.Run("OrderedByRecency", func(t *testing.T) {
		r := newRegistry(t)
		for i, id := range []string{"a", "b", "c"} {
			if err := r.Register(ctx, dev(id, "tok-"+id), t0.Add(time.Duration(i)*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		if got := ids(t, r, "bl", "u"); len(got) != 3 || got[0] != "c" || got[2] != "a" {
			t.Fatalf("order = %v, want c b a", got)
		}
		// 在前台使用过的设备排到最前；时间不倒退；未登记的设备忽略。
		if err := r.Touch(ctx, "bl", "u", "a", t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := r.Touch(ctx, "bl", "u", "a", t0); err != nil {
			t.Fatal(err)
		}
		if err := r.Touch(ctx, "bl", "u", "zz", t0.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		ds, _ := r.List(ctx, "bl", "u")
		if len(ds) != 3 || ds[0].DeviceID != "a" || !ds[0].LastActive.Equal(t0.Add(time.Hour)) {
			t.Fatalf("after touch: %+v", ds[0])
		}
	})

	t.Run("RegisterUpdatesTokenKeepsActivity", func(t *testing.T) {
		r := newRegistry(t)
		if err := r.Register(ctx, dev("a", "old"), t0); err != nil {
			t.Fatal(err)
		}
		if err := r.Touch(ctx, "bl", "u", "a", t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		d := dev("a", "new")
		d.Label = "iPhone"
		if err := r.Register(ctx, d, t0.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		ds, _ := r.List(ctx, "bl", "u")
		if len(ds) != 1 || ds[0].Token != "new" || ds[0].Label != "iPhone" || ds[0].Platform != "apns" || !ds[0].LastActive.Equal(t0.Add(time.Hour)) {
			t.Fatalf("re-registered device = %+v", ds[0])
		}
	})

	t.Run("ScopeAndDeletion", func(t *testing.T) {
		r := newRegistry(t)
		for _, d := range []*notify.Device{dev("a", "1"), dev("b", "2"),
			{BusinessLine: "bl", EndUser: "v", DeviceID: "a", Token: "3"}, {BusinessLine: "other", EndUser: "u", DeviceID: "a", Token: "4"}} {
			if err := r.Register(ctx, d, t0); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Unregister(ctx, "bl", "u", "b"); err != nil {
			t.Fatal(err)
		}
		if got := ids(t, r, "bl", "u"); len(got) != 1 || got[0] != "a" {
			t.Fatalf("after unregister: %v", got)
		}
		if err := r.DeleteEndUser(ctx, "bl", "u"); err != nil {
			t.Fatal(err)
		}
		if got := ids(t, r, "bl", "u"); len(got) != 0 {
			t.Fatalf("after deleting the end user: %v", got)
		}
		if len(ids(t, r, "bl", "v")) != 1 || len(ids(t, r, "other", "u")) != 1 {
			t.Fatal("deletion reached another end user or business line")
		}
	})
}
