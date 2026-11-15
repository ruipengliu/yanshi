// Package outboxtest 检查日志、队列、删除记录与出站信封的组合契约。
package outboxtest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/outbox"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
)

type Stores struct {
	Log       eventlog.Log
	Waking    eventlog.WakingLog
	Queue     workqueue.Queue
	Deletions lifecycle.Deletions
	Outbox    outbox.Store
}

func Run(t *testing.T, fresh func(*testing.T) Stores) {
	ctx := context.Background()
	list := func(t *testing.T, s Stores, bl string) []outbox.Envelope {
		t.Helper()
		rows, err := s.Outbox.List(ctx, bl)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	envelope := func(sid string, seq uint64) outbox.Envelope {
		return outbox.Envelope{EnvelopeID: outbox.ID(sid, seq, outbox.RunCompleted), BusinessLine: "bl", EndUser: "u",
			SessionID: sid, Seq: seq, Kind: outbox.RunCompleted, RunID: "r", Agent: "agent", AgentVersion: "v1"}
	}
	event := func() *v1.Event {
		return &v1.Event{Payload: &v1.Event_RunCompleted{RunCompleted: &v1.RunCompleted{RunId: "r", Attempt: 1}}}
	}
	class := workqueue.Class{BusinessLine: "bl", Priority: workqueue.Interactive}
	for _, wake := range []bool{false, true} {
		name := "Append"
		if wake {
			name = "AppendAndEnqueue"
		}
		t.Run(name, func(t *testing.T) {
			appendEvents := func(ctx context.Context, s Stores, sid string, seq uint64, env []outbox.Envelope, events ...*v1.Event) error {
				var err error
				if wake {
					_, err = s.Waking.AppendAndEnqueue(ctx, sid, seq, class, env, events...)
				} else {
					_, err = s.Log.Append(ctx, sid, seq, env, events...)
				}
				return err
			}
			assertEmpty := func(t *testing.T, s Stores) {
				t.Helper()
				events, err := s.Log.Read(ctx, "ses_v1_000_000_s", 0, 0)
				if err != nil || len(events) != 0 || len(list(t, s, "bl")) != 0 {
					t.Fatalf("partial commit: events=%d err=%v", len(events), err)
				}
				if _, err := s.Queue.Claim(ctx, "worker", time.Minute, workqueue.Pool{}); !errors.Is(err, workqueue.ErrEmpty) {
					t.Fatalf("queue changed: %v", err)
				}
			}
			t.Run("CommitConflictAndDuplicate", func(t *testing.T) {
				s := fresh(t)
				e := envelope("ses_v1_000_000_s", 1)
				if err := appendEvents(ctx, s, "ses_v1_000_000_s", 0, []outbox.Envelope{e}, event()); err != nil {
					t.Fatal(err)
				}
				if got := list(t, s, "bl"); !reflect.DeepEqual(got, []outbox.Envelope{e}) {
					t.Fatalf("envelopes = %+v", got)
				}
				for range 2 {
					if err := appendEvents(ctx, s, "ses_v1_000_000_s", 0, []outbox.Envelope{e}, event()); !errors.Is(err, eventlog.ErrConflict) {
						t.Fatalf("retry = %v", err)
					}
				}
				if got := list(t, s, "bl"); len(got) != 1 {
					t.Fatalf("retry duplicated envelope: %d", len(got))
				}
				events, err := s.Log.Read(ctx, "ses_v1_000_000_s", 0, 0)
				if err != nil || len(events) != 1 {
					t.Fatalf("events=%d err=%v", len(events), err)
				}
				lease, err := s.Queue.Claim(ctx, "worker", time.Minute, workqueue.Pool{})
				if wake && (err != nil || lease.SessionID != "ses_v1_000_000_s") {
					t.Fatalf("wake=%v %v", lease, err)
				}
				if !wake && !errors.Is(err, workqueue.ErrEmpty) {
					t.Fatalf("unexpected wake: %v", err)
				}
			})
			t.Run("ConcurrentConflict", func(t *testing.T) {
				s := fresh(t)
				start := make(chan struct{})
				results := make(chan error, 2)
				for _, run := range []string{"a", "b"} {
					go func() {
						<-start
						e := envelope("ses_v1_000_000_s", 1)
						e.RunID = run
						ev := event()
						ev.GetRunCompleted().RunId = run
						results <- appendEvents(ctx, s, "ses_v1_000_000_s", 0, []outbox.Envelope{e}, ev)
					}()
				}
				close(start)
				success, conflicts := 0, 0
				for range 2 {
					err := <-results
					if err == nil {
						success++
					} else if errors.Is(err, eventlog.ErrConflict) {
						conflicts++
					} else {
						t.Fatal(err)
					}
				}
				if success != 1 || conflicts != 1 {
					t.Fatalf("success=%d conflicts=%d", success, conflicts)
				}
				events, err := s.Log.Read(ctx, "ses_v1_000_000_s", 0, 0)
				got := list(t, s, "bl")
				if err != nil || len(events) != 1 || len(got) != 1 || events[0].GetRunCompleted().GetRunId() != got[0].RunID {
					t.Fatalf("conflicting commit leaked: events=%d envelopes=%+v err=%v", len(events), got, err)
				}
			})
			t.Run("RollbackWholeBatch", func(t *testing.T) {
				s := fresh(t)
				e := envelope("ses_v1_000_000_s", 1)
				// 第二条信封违反唯一约束：PG 已插事件和第一条信封，必须一起回滚。
				if err := appendEvents(ctx, s, "ses_v1_000_000_s", 0, []outbox.Envelope{e, e}, event()); err == nil {
					t.Fatal("duplicate accepted")
				}
				assertEmpty(t, s)
				if err := appendEvents(ctx, s, "ses_v1_000_000_s", 0, []outbox.Envelope{e}, event()); err != nil {
					t.Fatal(err)
				}
			})
			t.Run("CancelledAndUnserializable", func(t *testing.T) {
				s := fresh(t)
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				if err := appendEvents(cancelled, s, "ses_v1_000_000_s", 0, []outbox.Envelope{envelope("ses_v1_000_000_s", 1)}, event()); err == nil {
					t.Fatal("cancelled commit accepted")
				}
				assertEmpty(t, s)
				bad := event()
				bad.Id = string([]byte{0xff})
				if err := appendEvents(ctx, s, "ses_v1_000_000_s", 0, []outbox.Envelope{envelope("ses_v1_000_000_s", 1)}, event(), bad); err == nil {
					t.Fatal("invalid event accepted")
				}
				assertEmpty(t, s)
			})
			t.Run("RejectWrongSessionAndSequence", func(t *testing.T) {
				s := fresh(t)
				for _, e := range []outbox.Envelope{envelope("ses_v1_000_000_other", 1), envelope("ses_v1_000_000_s", 2), outbox.Deleted("bl", "ses_v1_000_000_s", "service", "")} {
					if err := appendEvents(ctx, s, "ses_v1_000_000_s", 0, []outbox.Envelope{e}, event()); err == nil {
						t.Fatal("foreign envelope accepted")
					}
				}
				assertEmpty(t, s)
			})
		})
	}
	t.Run("OrderIsolationAndCopies", func(t *testing.T) {
		s := fresh(t)
		a, b, c := envelope("ses_v1_000_000_a", 1), envelope("ses_v1_000_000_b", 1), envelope("ses_v1_000_000_c", 1)
		b.BusinessLine = "other"
		for _, e := range []outbox.Envelope{c, b, a} {
			if _, err := s.Log.Append(ctx, e.SessionID, 0, []outbox.Envelope{e}, event()); err != nil {
				t.Fatal(err)
			}
		}
		got := list(t, s, "bl")
		if !reflect.DeepEqual(got, []outbox.Envelope{c, a}) {
			t.Fatalf("order = %+v", got)
		}
		got[0].EndUser = "changed"
		if !reflect.DeepEqual(list(t, s, "bl"), []outbox.Envelope{c, a}) {
			t.Fatal("caller mutated stored envelope")
		}
		if !reflect.DeepEqual(list(t, s, "other"), []outbox.Envelope{b}) {
			t.Fatal("business line leak")
		}
	})
	t.Run("MarkAtomicIdempotentAndSurvivesDeletion", func(t *testing.T) {
		s := fresh(t)
		if _, err := s.Log.Append(ctx, "ses_v1_000_000_s", 0, []outbox.Envelope{envelope("ses_v1_000_000_s", 1)}, event()); err != nil {
			t.Fatal(err)
		}
		tomb := &lifecycle.Tombstone{SessionID: "ses_v1_000_000_s", BusinessLine: "bl", Reason: "account_deletion", RequestID: "request", RequestedAt: time.Unix(100, 0)}
		if err := s.Deletions.CreateRequest(ctx, &lifecycle.Request{ID: "request", BusinessLine: "bl", CreatedAt: tomb.RequestedAt}); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.Deletions.Mark(ctx, tomb); err != nil || !ok {
			t.Fatalf("mark=%v %v", ok, err)
		}
		want := outbox.Deleted("bl", "ses_v1_000_000_s", "account_deletion", "request")
		if got := list(t, s, "bl"); !reflect.DeepEqual(got, []outbox.Envelope{envelope("ses_v1_000_000_s", 1), want}) {
			t.Fatalf("mark envelopes=%+v", got)
		}
		changed := *tomb
		changed.Reason = "service"
		changed.RequestID = ""
		if ok, err := s.Deletions.Mark(ctx, &changed); err != nil || ok {
			t.Fatalf("duplicate mark=%v %v", ok, err)
		}
		for range 2 {
			if err := s.Outbox.DeleteSession(ctx, "ses_v1_000_000_s"); err != nil {
				t.Fatal(err)
			}
			if err := s.Log.Delete(ctx, "ses_v1_000_000_s"); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Deletions.Complete(ctx, "ses_v1_000_000_s", time.Unix(101, 0)); err != nil {
			t.Fatal(err)
		}
		if got := list(t, s, "bl"); !reflect.DeepEqual(got, []outbox.Envelope{want}) {
			t.Fatalf("deletion lost or changed=%+v", got)
		}
	})
	t.Run("FailedMarkLeavesNeitherRecord", func(t *testing.T) {
		s := fresh(t)
		tomb := &lifecycle.Tombstone{BusinessLine: "bl", Reason: "service", RequestedAt: time.Unix(100, 0)}
		// 空 Session 会在信封校验失败；PG 的删除记录已插入但还未提交。
		if ok, err := s.Deletions.Mark(ctx, tomb); err == nil || ok {
			t.Fatalf("bad mark=%v %v", ok, err)
		}
		if _, err := s.Deletions.Get(ctx, ""); !errors.Is(err, lifecycle.ErrNotFound) {
			t.Fatalf("mark leaked: %v", err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		tomb.SessionID = "ses_v1_000_000_s"
		if ok, err := s.Deletions.Mark(cancelled, tomb); err == nil || ok {
			t.Fatalf("cancelled mark=%v %v", ok, err)
		}
		if _, err := s.Deletions.Get(ctx, "ses_v1_000_000_s"); !errors.Is(err, lifecycle.ErrNotFound) {
			t.Fatalf("cancelled mark leaked: %v", err)
		}
		if got := list(t, s, "bl"); len(got) != 0 {
			t.Fatalf("mark leaked envelopes: %+v", got)
		}
	})
	t.Run("StoreSelectsOnlyValidatedEvents", func(t *testing.T) {
		s := fresh(t)
		store := &session.Store{Log: s.Log, Waking: s.Waking, Outbox: s.Outbox, Deletions: s.Deletions,
			IDs: ids.Sequential("e"), Clock: clock.Real{}}
		st := &session.State{SessionID: "ses_v1_000_000_s"}
		batch := []*v1.Event{
			{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u", Agent: &v1.AgentRef{Name: "agent", Version: "v1"}}}},
			{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r"}}},
			{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: "r", Attempt: 1}}},
			event(),
		}
		if err := store.Update(ctx, st, func(*session.State) (session.Decision, error) {
			return session.Decision{Events: batch, Wake: true}, nil
		}); err != nil {
			t.Fatal(err)
		}
		if got := list(t, s, "bl"); !reflect.DeepEqual(got, []outbox.Envelope{envelope("ses_v1_000_000_s", 4)}) {
			t.Fatalf("derived=%+v", got)
		}
		if err := store.Commit(ctx, st, event()); !errors.Is(err, session.ErrInvalid) {
			t.Fatalf("terminal run accepted event: %v", err)
		}
		if got := list(t, s, "bl"); len(got) != 1 {
			t.Fatalf("invalid event leaked: %+v", got)
		}
	})
	t.Run("StoreDerivesAndWithdrawsLateWrites", func(t *testing.T) {
		for _, lost := range []bool{false, true} {
			s := fresh(t)
			store := &session.Store{Log: s.Log, Waking: s.Waking, Outbox: s.Outbox, Deletions: s.Deletions,
				IDs: ids.Sequential("e"), Clock: clock.Real{}}
			st := &session.State{SessionID: "ses_v1_000_000_s"}
			base := []*v1.Event{
				{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u"}}},
				{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r"}}},
				{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: "r", Attempt: 1}}},
			}
			if err := store.Commit(ctx, st, base...); err != nil {
				t.Fatal(err)
			}
			// 模拟已通过前置检查的写入跨越 Mark 与清理，丢响应仍须撤回。
			if _, err := s.Deletions.Mark(ctx, &lifecycle.Tombstone{SessionID: "ses_v1_000_000_s", BusinessLine: "bl", Reason: "service", RequestedAt: time.Unix(100, 0)}); err != nil {
				t.Fatal(err)
			}
			if err := s.Outbox.DeleteSession(ctx, "ses_v1_000_000_s"); err != nil {
				t.Fatal(err)
			}
			if lost {
				store.Log = lostResponse{Log: s.Log}
			}
			err := store.Update(ctx, st, func(*session.State) (session.Decision, error) {
				return session.Decision{Events: []*v1.Event{event()}}, nil
			})
			if !errors.Is(err, session.ErrGone) {
				t.Fatalf("late commit=%v", err)
			}
			if got := list(t, s, "bl"); !reflect.DeepEqual(got, []outbox.Envelope{outbox.Deleted("bl", "ses_v1_000_000_s", "service", "")}) {
				t.Fatalf("late envelope survived=%+v", got)
			}
		}
	})
}

type lostResponse struct{ eventlog.Log }

func (l lostResponse) Append(ctx context.Context, sid string, seq uint64, envelopes []outbox.Envelope, events ...*v1.Event) (uint64, error) {
	_, err := l.Log.Append(ctx, sid, seq, envelopes, events...)
	if err != nil {
		return 0, err
	}
	return 0, errors.New("lost response")
}
