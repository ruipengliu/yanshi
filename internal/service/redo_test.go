package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"yanshi/internal/outbox"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/card"
	"yanshi/internal/eventlog"
	"yanshi/internal/model"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
)

type redoWaking struct {
	eventlog.WakingLog
	before func()
	lose   bool
	calls  int
}

func (w *redoWaking) AppendAndEnqueue(ctx context.Context, sid string, seq uint64, class workqueue.Class, envelopes []outbox.Envelope, events ...*v1.Event) (uint64, error) {
	if hook := w.before; hook != nil {
		w.before = nil
		hook()
	}
	n, err := w.WakingLog.AppendAndEnqueue(ctx, sid, seq, class, envelopes, events...)
	if err == nil {
		w.calls++
		if w.lose {
			w.lose = false
			return n, errors.New("lost response")
		}
	}
	return n, err
}

func redoFixture(t *testing.T, withCard bool) (*service.Service, string, string, *v1.CardAction) {
	t.Helper()
	s, _, sid := scheduleService(t, "UTC")
	res, err := s.SubmitWithID(t.Context(), sid, "first", model.TextBlocks("活动建议"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Load(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	events := []*v1.Event{{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: res.RunID, Attempt: 1}}}}
	var action *v1.CardAction
	if withCard {
		c, err := card.Parse(t.Context(), `{"card_id":"same-card","title":"建议","buttons":[{"id":"ok","label":"好"},{"id":"no","label":"不要"}]}`, "card-call", nil)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events,
			&v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{RunId: res.RunID, Attempt: 1,
				ToolCalls: []*v1.ToolCall{{CallId: "card-call", Capability: card.Capability, ArgumentsJson: "{}"}}}}},
			&v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{RunId: res.RunID, Attempt: 1, CallId: "card-call"}}},
			&v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{RunId: res.RunID, Attempt: 1, CallId: "card-call", Content: []*v1.ContentBlock{{Kind: &v1.ContentBlock_Card{Card: c}}}}}})
		action = &v1.CardAction{CardId: "same-card", CallId: "card-call", Action: "ok", Seq: st.Seq + uint64(len(events))}
	}
	events = append(events, &v1.Event{Payload: &v1.Event_RunCompleted{RunCompleted: &v1.RunCompleted{RunId: res.RunID, Attempt: 1}}})
	if err := s.Store.Commit(t.Context(), st, events...); err != nil {
		t.Fatal(err)
	}
	return s, sid, res.RunID, action
}

func TestRedoLostResponseAndDuplicateAfterClose(t *testing.T) {
	s, sid, original, _ := redoFixture(t, false)
	w := &redoWaking{WakingLog: s.Store.Waking, lose: true}
	s.Store.Waking = w
	if _, err := s.Redo(t.Context(), sid, "redo-id", original); err == nil {
		t.Fatal("expected lost response")
	}
	retry, err := s.Redo(t.Context(), sid, "redo-id", original)
	if err != nil || !retry.Duplicate || retry.Steered || w.calls != 1 {
		t.Fatalf("retry: %+v %v, wakes %d", retry, err, w.calls)
	}
	st, err := s.Load(t.Context(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if !st.SupersededRuns[original] || st.Active().Request.GetRedoOf() != original || !proto.Equal(&v1.RunRequested{Input: st.Active().Request.GetInput()}, &v1.RunRequested{Input: st.Run(original).Request.GetInput()}) {
		t.Fatal("redo did not copy and replace the original input")
	}
	if err := s.Close(t.Context(), sid, "user", "done"); err != nil {
		t.Fatal(err)
	}
	again, err := s.Redo(t.Context(), sid, "redo-id", "stale")
	if err != nil || !again.Duplicate || again.RunID != retry.RunID {
		t.Fatalf("duplicate after close: %+v %v", again, err)
	}
}

func TestRedoRechecksEveryDecision(t *testing.T) {
	for _, competitor := range []string{"redo", "same-id", "input", "schedule", "close"} {
		t.Run(competitor, func(t *testing.T) {
			s, sid, original, _ := redoFixture(t, false)
			if competitor == "schedule" {
				if _, err := s.CreateSchedule(t.Context(), sid, scheduleReq("due")); err != nil {
					t.Fatal(err)
				}
			}
			var winner *service.SubmitResult
			w := &redoWaking{WakingLog: s.Store.Waking}
			w.before = func() {
				var err error
				switch competitor {
				case "redo":
					winner, err = s.Redo(t.Context(), sid, "other-device", original)
				case "same-id":
					winner, err = s.Redo(t.Context(), sid, "redo-id", original)
				case "input":
					winner, err = s.SubmitWithID(t.Context(), sid, "other-input", model.TextBlocks("新问题"))
				case "close":
					err = s.Close(t.Context(), sid, "user", "done")
				case "schedule":
					st, loadErr := s.Load(t.Context(), sid)
					if loadErr != nil {
						t.Fatal(loadErr)
					}
					due := st.Schedules["due"].NextDue
					// 测试时钟前进到到期，使用真实 Fired/RunRequested 配对和 Store.Update。
					clk := s.Store.Clock.(interface{ Advance(time.Duration) })
					clk.Advance(due.Sub(s.Store.Clock.Now()))
					err = s.Store.Update(t.Context(), st, func(st *session.State) (session.Decision, error) {
						if st.Active() != nil {
							return session.Decision{}, nil
						}
						mark := &v1.ScheduleMark{ScheduleId: "due", DueAt: timestamppb.New(due)}
						return session.Decision{Events: []*v1.Event{
							{Payload: &v1.Event_ScheduleFired{ScheduleFired: &v1.ScheduleFired{ScheduleId: "due", DueAt: mark.DueAt, RunId: "scheduled"}}},
							{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "scheduled", InputId: session.ScheduleInputID("due", due), Input: st.Schedules["due"].Created.GetInput(), Schedule: mark}}},
						}}, nil
					})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			s.Store.Waking = w
			res, err := s.Redo(t.Context(), sid, "redo-id", original)
			if competitor == "same-id" {
				if err != nil || !res.Duplicate || res.RunID != winner.RunID {
					t.Fatalf("duplicate race: %+v %v", res, err)
				}
			} else if !errors.Is(err, service.ErrConflict) {
				t.Fatalf("stale expected run accepted: %+v %v", res, err)
			}
		})
	}
}

func TestInputRacingRedoBecomesSteer(t *testing.T) {
	s, sid, original, _ := redoFixture(t, false)
	var winner *service.SubmitResult
	w := &redoWaking{WakingLog: s.Store.Waking}
	w.before = func() {
		var err error
		winner, err = s.Redo(t.Context(), sid, "redo", original)
		if err != nil {
			t.Fatal(err)
		}
	}
	s.Store.Waking = w
	res, err := s.SubmitWithID(t.Context(), sid, "normal", model.TextBlocks("补充说明"))
	if err != nil || !res.Steered || res.RunID != winner.RunID {
		t.Fatalf("input race: %+v %v", res, err)
	}
}

func TestRedoInvalidatesCardActionsOnBothLookupPaths(t *testing.T) {
	for _, seq := range []bool{false, true} {
		for _, race := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "scan", true: "seq"}[seq], map[bool]string{false: "after", true: "race"}[race]}, "/"), func(t *testing.T) {
				s, sid, original, a := redoFixture(t, true)
				if !seq {
					a.Seq = 0
				}
				redo := func() {
					if _, err := s.Redo(t.Context(), sid, "redo", original); err != nil {
						t.Fatal(err)
					}
				}
				if race {
					s.Store.Waking = &redoWaking{WakingLog: s.Store.Waking, before: redo}
				} else {
					redo()
				}
				_, err := s.Submit(t.Context(), sid, []*v1.ContentBlock{{Kind: &v1.ContentBlock_CardAction{CardAction: a}}})
				if !errors.Is(err, service.ErrConflict) || !strings.Contains(err.Error(), "superseded_call") {
					t.Fatalf("old card action accepted: %v", err)
				}
			})
		}
	}
}

func TestCardActionBeforeRedoAndDuplicate(t *testing.T) {
	s, sid, original, a := redoFixture(t, true)
	input := []*v1.ContentBlock{{Kind: &v1.ContentBlock_CardAction{CardAction: a}}}
	first, err := s.Submit(t.Context(), sid, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Redo(t.Context(), sid, "redo", original); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("redo after card action: %v", err)
	}
	if err := s.Close(t.Context(), sid, "user", "done"); err != nil {
		t.Fatal(err)
	}
	// 重试不再核验字段值，也不因 Session 已关闭改变第一次的结果。
	a.Values = map[string]string{"unknown": "changed"}
	again, err := s.Submit(t.Context(), sid, input)
	if err != nil || !again.Duplicate || again.RunID != first.RunID {
		t.Fatalf("card duplicate: %+v %v", again, err)
	}
}

func TestClientCannotSubmitRedoOf(t *testing.T) {
	if err := service.CheckClientInput(nil, nil, "old-run"); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("client redo_of: %v", err)
	}
}
