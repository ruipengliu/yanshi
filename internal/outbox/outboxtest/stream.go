package outboxtest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/lifecycle"
	"yanshi/internal/outbox"
)

// Published 等待其他 schema 的短事务退出；xmin 是数据库级水位，不属于单个测试 schema。
func Published(t *testing.T, s outbox.Reader, req outbox.Request, count int) outbox.Page {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		page, err := s.Read(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Events) == count {
			return page
		}
		select {
		case <-ctx.Done():
			t.Fatalf("published %d events, want %d", len(page.Events), count)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func RunStream(t *testing.T, fresh func(*testing.T) Stores) {
	t.Run("DeletionRetention", func(t *testing.T) { RunDeletionRetention(t, fresh) })
	ctx := context.Background()
	appendRows := func(t *testing.T, s Stores, bl, sid string, kinds ...outbox.Kind) []outbox.Envelope {
		t.Helper()
		var env []outbox.Envelope
		var events []*v1.Event
		for i, kind := range kinds {
			seq := uint64(i + 1)
			env = append(env, outbox.Envelope{EnvelopeID: outbox.ID(sid, seq, kind), BusinessLine: bl,
				SessionID: sid, Seq: seq, Kind: kind})
			events = append(events, &v1.Event{Payload: &v1.Event_RunCompleted{RunCompleted: &v1.RunCompleted{RunId: "r"}}})
		}
		if _, err := s.Log.Append(ctx, sid, 0, env, events...); err != nil {
			t.Fatal(err)
		}
		return env
	}
	read := func(t *testing.T, s Stores, req outbox.Request) outbox.Page {
		t.Helper()
		page, err := s.Outbox.Read(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	t.Run("PaginationFilterAndSafeHead", func(t *testing.T) {
		s := fresh(t)
		req := outbox.Request{BusinessLine: "bl", Limit: 1}
		h, err := s.Outbox.Head(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		want := appendRows(t, s, "bl", "s", outbox.RunCompleted, outbox.Question, outbox.RunFailed)
		appendRows(t, s, "other", "foreign", outbox.Question)
		Published(t, s.Outbox, outbox.Request{BusinessLine: "bl"}, 3)
		stats, err := s.Outbox.Stats(ctx)
		if err != nil || stats.Rows != 4 || stats.Bytes <= 0 {
			t.Fatalf("retained space=%+v %v", stats, err)
		}
		req.After = h
		for _, e := range want {
			page := read(t, s, req)
			if !reflect.DeepEqual(page.Events, []outbox.Envelope{e}) {
				t.Fatalf("page=%+v", page)
			}
			req.After = page.Next
		}
		if page := read(t, s, req); len(page.Events) != 0 || page.Next == "" {
			t.Fatalf("tail=%+v", page)
		}
		filtered := read(t, s, outbox.Request{BusinessLine: "bl", Kinds: []outbox.Kind{outbox.Question}})
		if !reflect.DeepEqual(filtered.Events, []outbox.Envelope{want[1]}) {
			t.Fatal("filter or tenant isolation failed")
		}
		_, err = s.Outbox.Read(ctx, outbox.Request{BusinessLine: "bl", After: filtered.Next})
		if !errors.Is(err, outbox.ErrFilter) {
			t.Fatalf("filter change=%v", err)
		}
		_, err = s.Outbox.Read(ctx, outbox.Request{BusinessLine: "other", After: filtered.Next})
		if !errors.Is(err, outbox.ErrScope) {
			t.Fatalf("scope change=%v", err)
		}
		canonical := outbox.Request{BusinessLine: "bl", Kinds: []outbox.Kind{outbox.Question, outbox.RunCompleted}}
		page := read(t, s, canonical)
		canonical.After, canonical.Kinds = page.Next, []outbox.Kind{outbox.RunCompleted, outbox.Question, outbox.Question}
		read(t, s, canonical)
	})
	t.Run("EmptyProgressAndValidation", func(t *testing.T) {
		s := fresh(t)
		req := outbox.Request{BusinessLine: "bl", Kinds: []outbox.Kind{outbox.Question}}
		first := read(t, s, req)
		appendRows(t, s, "bl", "s", outbox.RunCompleted)
		Published(t, s.Outbox, outbox.Request{BusinessLine: "bl"}, 1)
		req.After = first.Next
		page := read(t, s, req)
		if len(page.Events) != 0 || page.Next == first.Next {
			t.Fatal("empty filtered page did not advance")
		}
		appendRows(t, s, "other", "foreign", outbox.RunCompleted)
		Published(t, s.Outbox, outbox.Request{BusinessLine: "other"}, 1)
		req.After = page.Next
		foreignProgress := read(t, s, req)
		if len(foreignProgress.Events) != 0 || foreignProgress.Next == page.Next {
			t.Fatal("empty tenant page did not advance")
		}
		for _, bad := range []outbox.Request{
			{BusinessLine: "bl", After: "bad"}, {BusinessLine: "bl", After: page.Next + "x"},
			{BusinessLine: "bl", Limit: 1001}, {BusinessLine: "bl", Kinds: []outbox.Kind{"bad"}},
			{BusinessLine: "bl", Shard: 1}, {BusinessLine: "bl", After: strings.Repeat("x", 5000)},
		} {
			if _, err := s.Outbox.Read(ctx, bad); err == nil {
				t.Fatal("invalid request accepted")
			}
		}
		rotate := s.Outbox.(interface{ RotateEpoch(context.Context) error })
		if err := rotate.RotateEpoch(ctx); err != nil {
			t.Fatal(err)
		}
		req.After = page.Next
		if _, err := s.Outbox.Read(ctx, req); !errors.Is(err, outbox.ErrEpoch) {
			t.Fatalf("old epoch=%v", err)
		}
		// 新世代从空游标开始，不复用旧检查点。
		req.After = ""
		read(t, s, req)
	})
	t.Run("RetentionObservationBatchesAndEmptyFloor", func(t *testing.T) {
		s := fresh(t)
		req := outbox.Request{BusinessLine: "bl"}
		old := read(t, s, req)
		now := time.Unix(1000, 0)
		if err := s.Outbox.Maintain(ctx, clock.NewFake(now), 1); err != nil {
			t.Fatal(err)
		}
		appendRows(t, s, "bl", "a", outbox.RunCompleted, outbox.Question)
		appendRows(t, s, "bl", "b", outbox.RunCompleted)
		Published(t, s.Outbox, req, 3)
		// 旧检查点不覆盖随后才可发布的行；经过四天也不能按写入时间删。
		if err := s.Outbox.Maintain(ctx, clock.NewFake(now.Add(96*time.Hour)), 1); err != nil {
			t.Fatal(err)
		}
		if page := read(t, s, req); len(page.Events) != 3 {
			t.Fatal("premature retention")
		}
		at := now.Add(168 * time.Hour)
		if err := s.Outbox.Maintain(ctx, clock.NewFake(at), 1); err != nil {
			t.Fatal(err)
		}
		req.After = old.Next
		if _, err := s.Outbox.Read(ctx, req); !errors.Is(err, outbox.ErrExpired) {
			t.Fatalf("partial cleanup cursor=%v", err)
		}
		for range 4 {
			if err := s.Outbox.Maintain(ctx, clock.NewFake(at), 1); err != nil {
				t.Fatal(err)
			}
		}
		if rows, err := s.Outbox.List(ctx, "bl"); err != nil || len(rows) != 0 {
			t.Fatalf("remaining=%d %v", len(rows), err)
		}
		if _, err := s.Outbox.Read(ctx, req); !errors.Is(err, outbox.ErrExpired) {
			t.Fatalf("empty floor=%v", err)
		}
		req.After = ""
		page := read(t, s, req)
		if len(page.Events) != 0 || page.Next == "" {
			t.Fatal("fresh consumer cannot start at floor")
		}
	})
	t.Run("SessionDeletionDoesNotInventFloor", func(t *testing.T) {
		s := fresh(t)
		req := outbox.Request{BusinessLine: "bl"}
		old := read(t, s, req)
		appendRows(t, s, "bl", "s", outbox.Question)
		Published(t, s.Outbox, req, 1)
		if err := s.Outbox.DeleteSession(ctx, "s"); err != nil {
			t.Fatal(err)
		}
		req.After = old.Next
		read(t, s, req)
		now := time.Unix(1000, 0)
		for _, at := range []time.Time{now, now.Add(outbox.Retention)} {
			if err := s.Outbox.Maintain(ctx, clock.NewFake(at), 100); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Outbox.Read(ctx, req); !errors.Is(err, outbox.ErrExpired) {
			t.Fatalf("empty table lost floor=%v", err)
		}
	})
}

// RunDeletionRetention 独立验证删除信封不随 Session 清除，但与普通信封使用相同保留期。
func RunDeletionRetention(t *testing.T, fresh func(*testing.T) Stores) {
	s := fresh(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	tomb := &lifecycle.Tombstone{SessionID: "s", BusinessLine: "bl", Reason: "service", RequestedAt: now}
	if _, err := s.Deletions.Mark(ctx, tomb); err != nil {
		t.Fatal(err)
	}
	Published(t, s.Outbox, outbox.Request{BusinessLine: "bl"}, 1)
	if err := s.Outbox.DeleteSession(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if err := s.Outbox.Maintain(ctx, clock.NewFake(now), 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Outbox.Maintain(ctx, clock.NewFake(now.Add(outbox.Retention-time.Second)), 1); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.Outbox.List(ctx, "bl"); err != nil || len(rows) != 1 {
		t.Fatal("deletion envelope pruned early")
	}
	if err := s.Outbox.Maintain(ctx, clock.NewFake(now.Add(outbox.Retention)), 1); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.Outbox.List(ctx, "bl"); err != nil || len(rows) != 0 {
		t.Fatal("deletion envelope not pruned")
	}
}
