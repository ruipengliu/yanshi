package session_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

func scheduleProjection(t *testing.T) (*session.Store, *session.State, *v1.ScheduleCreated, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC))
	store := &session.Store{Log: memlog.New(), Clock: clk, IDs: ids.Sequential("s")}
	st := &session.State{SessionID: "ses_v1_000_000_s"}
	if err := store.Commit(context.Background(), st, &v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{TimeZone: "UTC"}}}); err != nil {
		t.Fatal(err)
	}
	m := &v1.ScheduleCreated{ScheduleId: "one", Input: model.TextBlocks("提醒"), TimeZone: "UTC", At: "2026-10-28T01:00:00", Start: timestamppb.New(clk.Now()), NextDue: timestamppb.New(clk.Now().Add(time.Hour)), By: "service"}
	return store, st, m, clk
}

func TestScheduleDeclarationProjection(t *testing.T) {
	mutations := map[string]func(*v1.ScheduleCreated){
		"empty id":   func(m *v1.ScheduleCreated) { m.ScheduleId = "" },
		"newline id": func(m *v1.ScheduleCreated) { m.ScheduleId = "one\nuser" },
		"no text":    func(m *v1.ScheduleCreated) { m.Input = nil },
		"media": func(m *v1.ScheduleCreated) {
			m.Input = []*v1.ContentBlock{{Kind: &v1.ContentBlock_Media{Media: &v1.Media{}}}}
		},
		"zone":     func(m *v1.ScheduleCreated) { m.TimeZone = "" },
		"spec":     func(m *v1.ScheduleCreated) { m.Rrule = "FREQ=DAILY" },
		"no start": func(m *v1.ScheduleCreated) { m.Start = nil },
		"next":     func(m *v1.ScheduleCreated) { m.NextDue = m.Start },
		"by":       func(m *v1.ScheduleCreated) { m.By = "token" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			store, st, m, _ := scheduleProjection(t)
			mutate(m)
			if err := store.Commit(context.Background(), st, &v1.Event{Payload: &v1.Event_ScheduleCreated{ScheduleCreated: m}}); err == nil {
				t.Fatal("invalid declaration accepted")
			}
			if st.Seq != 1 {
				t.Fatal("failed Apply mutated state")
			}
		})
	}
	store, st, m, _ := scheduleProjection(t)
	e := &v1.Event{Payload: &v1.Event_ScheduleCreated{ScheduleCreated: m}}
	if err := store.Commit(context.Background(), st, e); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(context.Background(), st, e); err == nil {
		t.Fatal("duplicate accepted")
	}
	for i := 1; i < session.MaxSchedules; i++ {
		c := proto.Clone(m).(*v1.ScheduleCreated)
		c.ScheduleId = fmt.Sprint(i)
		if err := store.Commit(context.Background(), st, &v1.Event{Payload: &v1.Event_ScheduleCreated{ScheduleCreated: c}}); err != nil {
			t.Fatal(err)
		}
	}
	c := proto.Clone(m).(*v1.ScheduleCreated)
	c.ScheduleId = "overflow"
	if err := store.Commit(context.Background(), st, &v1.Event{Payload: &v1.Event_ScheduleCreated{ScheduleCreated: c}}); err == nil {
		t.Fatal("limit accepted")
	}
	restored, err := session.FromSnapshot(st.Snapshot())
	if err != nil || !session.Equal(st, restored) {
		t.Fatalf("snapshot: %v", err)
	}
}

func TestSchedulePairValidation(t *testing.T) {
	for _, kind := range []string{"valid", "unpaired fire", "unpaired mark", "wrong input", "wrong input id", "wrong due", "wrong next", "wrong run", "from call", "active", "replay"} {
		t.Run(kind, func(t *testing.T) {
			store, st, m, clk := scheduleProjection(t)
			ctx := context.Background()
			if err := store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_ScheduleCreated{ScheduleCreated: m}}); err != nil {
				t.Fatal(err)
			}
			clk.Advance(time.Hour)
			f := &v1.ScheduleFired{ScheduleId: "one", DueAt: m.NextDue, RunId: "r"}
			r := &v1.RunRequested{RunId: "r", Input: m.Input, InputId: session.ScheduleInputID("one", clk.Now()), Schedule: &v1.ScheduleMark{ScheduleId: "one", DueAt: m.NextDue}}
			events := []*v1.Event{{Payload: &v1.Event_ScheduleFired{ScheduleFired: f}}, {Payload: &v1.Event_RunRequested{RunRequested: r}}}
			switch kind {
			case "unpaired fire":
				events = events[:1]
			case "unpaired mark":
				events = events[1:]
			case "wrong input":
				r.Input = model.TextBlocks("伪造")
			case "wrong input id":
				r.InputId = "client"
			case "wrong due":
				f.DueAt = timestamppb.New(clk.Now().Add(-time.Second))
			case "wrong next":
				f.NextDue = f.DueAt
			case "wrong run":
				r.RunId = "other"
			case "from call":
				r.FromCall = "call"
			case "active":
				if err := store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "user"}}}); err != nil {
					t.Fatal(err)
				}
			}
			err := store.Commit(ctx, st, events...)
			if kind == "valid" || kind == "replay" {
				if err != nil {
					t.Fatal(err)
				}
				restored, err := session.FromSnapshot(st.Snapshot())
				if err != nil || !session.Equal(st, restored) {
					t.Fatalf("snapshot: %v", err)
				}
				if kind == "replay" {
					if err := store.Commit(ctx, st, events...); err == nil {
						t.Fatal("replay accepted")
					}
				}
			} else if err == nil {
				t.Fatal("invalid pair accepted")
			}
		})
	}
}

func TestScheduleSkippedValidation(t *testing.T) {
	for _, kind := range []string{"valid", "first", "last", "count", "reason", "next", "closed"} {
		t.Run(kind, func(t *testing.T) {
			store, st, m, clk := scheduleProjection(t)
			ctx := context.Background()
			m.At = ""
			m.Rrule = "FREQ=DAILY;BYHOUR=1,2,3;BYMINUTE=0"
			if err := store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_ScheduleCreated{ScheduleCreated: m}}); err != nil {
				t.Fatal(err)
			}
			clk.Advance(3 * time.Hour)
			sk := &v1.ScheduleSkipped{ScheduleId: "one", FirstDue: m.NextDue, LastDue: timestamppb.New(clk.Now().Add(-time.Hour)), Count: 2, Reason: "missed", NextDue: timestamppb.New(clk.Now())}
			switch kind {
			case "first":
				sk.FirstDue = sk.LastDue
			case "last":
				sk.LastDue = timestamppb.New(sk.FirstDue.AsTime().Add(-time.Second))
			case "count":
				sk.Count = 0
			case "reason":
				sk.Reason = "quota"
			case "next":
				sk.NextDue = sk.LastDue
			case "closed":
				sk.Reason = "closed"
				sk.NextDue = nil
			}
			err := store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_ScheduleSkipped{ScheduleSkipped: sk}})
			if kind == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				restored, err := session.FromSnapshot(st.Snapshot())
				if err != nil || !session.Equal(st, restored) {
					t.Fatalf("skipped snapshot: %v", err)
				}
			} else if err == nil {
				t.Fatal("invalid skipped accepted")
			}
		})
	}
}

func TestScheduleCannotBeCreatedInChildOrClosedSession(t *testing.T) {
	for _, child := range []bool{true, false} {
		store, st, m, _ := scheduleProjection(t)
		if child {
			st.Created = proto.Clone(st.Created).(*v1.SessionCreated)
			st.Created.Parent = &v1.ParentRef{SessionId: "ses_v1_000_000_parent"}
		} else {
			if err := store.Commit(context.Background(), st, &v1.Event{Payload: &v1.Event_SessionClosed{SessionClosed: &v1.SessionClosed{By: "service"}}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.Commit(context.Background(), st, &v1.Event{Payload: &v1.Event_ScheduleCreated{ScheduleCreated: m}}); err == nil {
			t.Fatal("created schedule in child or closed Session")
		}
	}
}

func TestScheduleSnapshotRejectsPendingPairAndPreservesCancellation(t *testing.T) {
	store, st, m, clk := scheduleProjection(t)
	ctx := context.Background()
	if err := store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_ScheduleCreated{ScheduleCreated: m}}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	f := &v1.Event{SessionId: st.SessionID, Seq: st.Seq + 1, Time: timestamppb.New(clk.Now()), Payload: &v1.Event_ScheduleFired{ScheduleFired: &v1.ScheduleFired{ScheduleId: "one", DueAt: m.NextDue, RunId: "r"}}}
	prefix := st.Clone()
	if err := prefix.Apply(f); err != nil {
		t.Fatal(err)
	}
	if prefix.Snapshot() != nil {
		t.Fatal("snapshot accepted an unpaired Fired")
	}
	if session.Equal(prefix, prefix) {
		t.Fatal("invalid snapshots compared equal")
	}
	if err := store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_ScheduleCancelled{ScheduleCancelled: &v1.ScheduleCancelled{ScheduleId: "one", By: "end_user"}}}); err != nil {
		t.Fatal(err)
	}
	restored, err := session.FromSnapshot(st.Snapshot())
	if err != nil || !session.Equal(st, restored) {
		t.Fatalf("cancelled snapshot: %v", err)
	}
	if len(restored.OpenSchedules()) != 0 || restored.Schedules["one"] == nil {
		t.Fatal("snapshot lost cancelled declaration")
	}
}

// 历史 UTC 即使与当前规则或时区目录不同，也必须能完整重放。
func TestScheduleReplayUsesPersistedUTC(t *testing.T) {
	for _, zone := range []string{"UTC", "Retired/Zone"} {
		t.Run(zone, func(t *testing.T) {
			store, st, m, clk := scheduleProjection(t)
			ctx := context.Background()
			m.TimeZone = zone
			m.NextDue = timestamppb.New(clk.Now().Add(90 * time.Minute))
			if err := store.Commit(ctx, st, &v1.Event{Payload: &v1.Event_ScheduleCreated{ScheduleCreated: m}}); err != nil {
				t.Fatal(err)
			}
			clk.Advance(90 * time.Minute)
			if err := store.Commit(ctx, st,
				&v1.Event{Payload: &v1.Event_ScheduleFired{ScheduleFired: &v1.ScheduleFired{ScheduleId: "one", DueAt: m.NextDue, RunId: "r"}}},
				&v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r", Input: m.Input, InputId: session.ScheduleInputID("one", clk.Now()), Schedule: &v1.ScheduleMark{ScheduleId: "one", DueAt: m.NextDue}}}}); err != nil {
				t.Fatal(err)
			}
			events, err := eventlog.ReadAll(ctx, store.Log, st.SessionID, 0)
			if err != nil {
				t.Fatal(err)
			}
			// Session 的历史时区也不能在 Apply 中重新 LoadLocation。
			events[0] = proto.Clone(events[0]).(*v1.Event)
			events[0].GetSessionCreated().TimeZone = "Retired/Zone"
			replayed, err := session.Reduce(events)
			if err != nil || replayed.Active() == nil {
				t.Fatalf("replay: %v", err)
			}
			if _, err := session.Reduce(events[:len(events)-1]); err == nil {
				t.Fatal("complete log accepted a dangling Fired")
			}
		})
	}
}
