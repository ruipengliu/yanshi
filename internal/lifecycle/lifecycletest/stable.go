package lifecycletest

import (
	"context"
	"slices"
	"testing"
	"time"

	"yanshi/internal/lifecycle"
)

func runStableIndex(t *testing.T, x lifecycle.Index) {
	ctx := context.Background()
	put := func(id, line, user string) {
		t.Helper()
		if err := x.Put(ctx, &lifecycle.Session{ID: sid(id), BusinessLine: line, EndUser: user, Agent: "a", CreatedAt: t0, LastInputAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"a", "c", "e", "g", "z", "Z", "a-1", "a_1"} {
		put(id, "bl", "u")
	}
	put("other-owner", "bl", "v")
	put("other-line", "other", "u")
	if err := x.MarkClosed(ctx, "u", sid("e"), t0); err != nil {
		t.Fatal(err)
	}
	page, next, err := x.ListStable(ctx, "bl", "u", "", 2)
	if err != nil || len(page) != 2 || page[0].ID != sid("Z") || page[1].ID != sid("a") || next != sid("a") {
		t.Fatalf("first page: %+v %q %v", page, next, err)
	}
	// 把未扫描项移到产品列表前方，稳定列表仍须找到它。
	if err := x.Touch(ctx, "u", sid("e"), t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 两页之间创建与删除，删除游标行不能使后页退回或跳过仍可见项。
	put("b", "bl", "u")
	put("0-new", "bl", "u") // 落在已扫区间的新项由 H 后追赶兜底，不宣称快照。
	for _, id := range []string{"a", "g"} {
		if err := x.Delete(ctx, "u", sid(id)); err != nil {
			t.Fatal(err)
		}
	}
	got := sids("Z", "a")
	closed := false
	for range 10 {
		page, cursor, err := x.ListStable(ctx, "bl", "u", next, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range page {
			got = append(got, row.ID)
			if row.ID == sid("e") {
				closed = !row.ClosedAt.IsZero()
			}
		}
		if cursor == "" {
			break
		}
		next = cursor
	}
	if !slices.Equal(got, sids("Z", "a", "a-1", "a_1", "b", "c", "e", "z")) || !closed {
		t.Fatalf("stable scan: %v, closed=%v", got, closed)
	}
	for _, tc := range []struct {
		line, user string
		limit      int
	}{{"", "u", 1}, {"bl", "", 1}, {"bl", "u", 0}, {"bl", "u", 201}} {
		if _, _, err := x.ListStable(ctx, tc.line, tc.user, "", tc.limit); err == nil {
			t.Fatal("invalid stable scope/limit accepted")
		}
	}
	rows, _, err := x.ListStable(ctx, "bl", "u", "", 200)
	if err != nil || len(rows) != 8 || rows[0].ID != sid("0-new") {
		t.Fatalf("fresh scan: %+v %v", rows, err)
	}
}

// RunStableDeletion 区分逻辑删除与 Janitor 清索引，读者不能把列表当可访问性证明。
func RunStableDeletion(t *testing.T, x lifecycle.Index, d lifecycle.Deletions) {
	ctx := context.Background()
	if err := x.Put(ctx, &lifecycle.Session{ID: sid("deleted"), BusinessLine: "bl", EndUser: "u", CreatedAt: t0, LastInputAt: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Mark(ctx, &lifecycle.Tombstone{SessionID: sid("deleted"), BusinessLine: "bl", RequestedAt: t0}); err != nil {
		t.Fatal(err)
	}
	rows, _, err := x.ListStable(ctx, "bl", "u", "", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("before janitor: %+v %v", rows, err)
	}
	if err := x.Delete(ctx, "u", sid("deleted")); err != nil {
		t.Fatal(err)
	}
	rows, next, err := x.ListStable(ctx, "bl", "u", "", 10)
	if err != nil || len(rows) != 0 || next != "" {
		t.Fatalf("after janitor: %+v %q %v", rows, next, err)
	}
}
