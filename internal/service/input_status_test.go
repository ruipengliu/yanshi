package service_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/artifact"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/model"
	"yanshi/internal/model/script"
	"yanshi/internal/node"
	"yanshi/internal/runtime"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
)

func TestInputStatusOutsideDuplicateWindow(t *testing.T) {
	s, _, sid := scheduleService(t, "UTC")
	ctx := t.Context()
	first, err := s.SubmitWithID(ctx, sid, "old", model.TextBlocks("开始"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Interrupt(ctx, sid, first.RunID); err != nil {
		t.Fatal(err)
	}
	for i := range 65 {
		sub, err := s.SubmitWithID(ctx, sid, fmt.Sprint(i), model.TextBlocks("继续"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Interrupt(ctx, sid, sub.RunID); err != nil {
			t.Fatal(err)
		}
	}
	st, err := s.Load(ctx, sid)
	if err != nil || st.Input("old") != nil || st.Run(first.RunID) != nil {
		t.Fatalf("fixture did not evict input and Run: %v", err)
	}
	got, err := s.InputStatus(ctx, sid, "old")
	if err != nil || !got.Found || got.RunID != first.RunID || got.RunStatus != session.RunInterrupted ||
		got.AcceptanceKind != "requested" || got.AcceptedSeq != 2 || got.EventID == "" || got.AsOfSeq != st.Seq {
		t.Fatalf("old input status: %+v, %v", got, err)
	}
	if _, err := s.SubmitWithID(ctx, sid, "old", model.TextBlocks("再次提交")); err != nil {
		t.Fatal(err)
	}
	_, err = s.InputStatus(ctx, sid, "old")
	var reason *service.ReasonError
	if !errors.Is(err, service.ErrConflict) || !errors.As(err, &reason) || reason.Reason != "input_accepted_more_than_once" {
		t.Fatalf("duplicate acceptance: %v", err)
	}
}

func TestInputStatusPlatformIDs(t *testing.T) {
	s, clk, sid := scheduleService(t, "UTC")
	ctx := t.Context()
	item, err := s.CreateSchedule(ctx, sid, scheduleReq("tick"))
	if err != nil {
		t.Fatal(err)
	}
	due := item.NextDue
	clk.Advance(due.Sub(clk.Now()))
	st, err := s.Load(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FireSchedule(ctx, st, "tick"); err != nil {
		t.Fatal(err)
	}
	checkMissing := func(sid, id string) {
		t.Helper()
		got, err := s.InputStatus(ctx, sid, id)
		if err != nil || got.Found || got.AsOfSeq == 0 {
			t.Fatalf("platform input status: %+v, %v", got, err)
		}
	}
	checkMissing(sid, session.ScheduleInputID("tick", due))
	child, err := s.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a",
		Parent: &v1.ParentRef{SessionId: sid, RunId: "parent-run", CallId: "delegation-call"}})
	if err != nil {
		t.Fatal(err)
	}
	delegationID := session.DelegatedInputID("delegation-call")
	if _, err := s.SubmitDelegated(ctx, child, delegationID, model.TextBlocks("委托"), nil); err != nil {
		t.Fatal(err)
	}
	checkMissing(child, delegationID)
	if _, err := s.SubmitWithID(ctx, sid, delegationID, model.TextBlocks("普通插话")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.InputStatus(ctx, sid, delegationID); err != nil || !got.Found || got.AcceptanceKind != "steered" {
		t.Fatalf("client input with the same prefix: %+v, %v", got, err)
	}
}

type inputStatusReadHook struct {
	eventlog.Log
	beforePage func()
}

func (l inputStatusReadHook) Read(ctx context.Context, sid string, after uint64, limit int) ([]*v1.Event, error) {
	if after == 0 && limit > 0 {
		l.beforePage()
	}
	return l.Log.Read(ctx, sid, after, limit)
}

func TestInputStatusReadRaces(t *testing.T) {
	for _, deleteDuringScan := range []bool{false, true} {
		t.Run(fmt.Sprint(deleteDuringScan), func(t *testing.T) {
			s, clk, sid := scheduleService(t, "UTC")
			ctx := t.Context()
			s.Deletions = lifecycle.NewMemDeletions()
			st, err := s.Load(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			log := s.Store.Log
			s.Store.Log = inputStatusReadHook{Log: log, beforePage: func() {
				if deleteDuringScan {
					if _, err := s.Deletions.Mark(ctx, &lifecycle.Tombstone{SessionID: sid, BusinessLine: "bl", RequestedAt: clk.Now()}); err != nil {
						t.Fatal(err)
					}
					if err := log.Delete(ctx, sid); err != nil {
						t.Fatal(err)
					}
					return
				}
				e := &v1.Event{Id: "late", Time: timestamppb.New(clk.Now()), Payload: &v1.Event_RunRequested{
					RunRequested: &v1.RunRequested{RunId: "late-run", InputId: "late", Input: model.TextBlocks("新输入")}}}
				if _, err := log.Append(ctx, sid, st.Seq, nil, e); err != nil {
					t.Fatal(err)
				}
			}}
			got, err := s.InputStatus(ctx, sid, "late")
			if deleteDuringScan {
				if !errors.Is(err, service.ErrNotFound) {
					t.Fatalf("deleted during scan: %+v, %v", got, err)
				}
				if _, err := s.InputStatus(ctx, sid, "late"); !errors.Is(err, service.ErrNotFound) {
					t.Fatalf("deleted before lookup: %v", err)
				}
			} else if err != nil || got.Found || got.AsOfSeq != st.Seq {
				t.Fatalf("read past projection: %+v, %v", got, err)
			}
		})
	}
}

// 用模拟的 Worker 步骤生成多种已提交的历史，再用独立的按调用归约核对每个读取上界。
func TestInputStatusReplayAgreement(t *testing.T) {
	for _, finish := range []string{"finish", "failed", "suspend"} {
		t.Run(finish, func(t *testing.T) {
			s, clk, sid := scheduleService(t, "UTC")
			ctx := t.Context()
			sub, err := s.SubmitWithID(ctx, sid, "start", model.TextBlocks("开始"))
			if err != nil {
				t.Fatal(err)
			}
			run := sub.RunID
			st, err := s.Load(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			commit := func(events ...*v1.Event) {
				t.Helper()
				if err := s.Store.Commit(ctx, st, events...); err != nil {
					t.Fatal(err)
				}
			}
			attempt := func(n uint32) *v1.Event {
				return &v1.Event{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: run, Attempt: n}}}
			}
			start := func(n uint32, id string, readOnly bool) *v1.Event {
				return &v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{
					RunId: run, Attempt: n, CallId: id, ReadOnly: readOnly, NodeId: "device"}}}
			}
			result := func(n uint32, id string, isError bool, bg *v1.Backgrounded) *v1.Event {
				return &v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{
					RunId: run, Attempt: n, CallId: id, IsError: isError, Backgrounded: bg}}}
			}
			check := func(id string, want service.InputCallEvidence, status session.RunStatus, attempts, takeovers uint32) {
				t.Helper()
				got, err := s.InputStatus(ctx, sid, id)
				if err != nil || !got.Found || got.Calls != want || got.RunStatus != status ||
					got.AttemptCount != attempts || got.TakeoverCount != takeovers {
					t.Fatalf("status for %s: %+v, %v", id, got, err)
				}
				events, err := s.Store.Log.Read(ctx, sid, 0, int(got.AsOfSeq))
				if err != nil || uint64(len(events)) != got.AsOfSeq {
					t.Fatalf("reference read: %d, %v", len(events), err)
				}
				if ref := referenceInputStatus(t, events, id); !reflect.DeepEqual(got, ref) {
					t.Fatalf("query: %+v\nreference: %+v", got, ref)
				}
			}
			check("start", service.InputCallEvidence{}, session.RunQueued, 0, 0)
			commit(attempt(1), &v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{
				RunId: run, Attempt: 1, ToolCalls: []*v1.ToolCall{
					{CallId: "bad", Capability: "pc__write"}, {CallId: "bg", Capability: "pc__write"},
					{CallId: "read", Capability: "pc__read"}, {CallId: "pending", Capability: "pc__write"},
				}}}}, start(1, "bad", false), result(1, "bad", true, nil),
				start(1, "bg", false), result(1, "bg", false, &v1.Backgrounded{NodeId: "device", Deadline: timestamppb.New(clk.Now())}),
				start(1, "read", true), result(1, "read", true, nil), start(1, "pending", false),
				&v1.Event{Payload: &v1.Event_RunSuspended{RunSuspended: &v1.RunSuspended{RunId: run, Attempt: 1}}})
			steer, err := s.SubmitWithID(ctx, sid, "steer", model.TextBlocks("插话"))
			if err != nil || !steer.Steered {
				t.Fatalf("steer: %+v, %v", steer, err)
			}
			calls := service.InputCallEvidence{WritesWithError: 1, WritesBackgrounded: 1, WritesWithoutResult: 1}
			check("steer", calls, session.RunSuspended, 1, 0)
			if finish == "suspend" {
				return
			}
			if err := s.Store.Sync(ctx, st); err != nil {
				t.Fatal(err)
			}
			commit(attempt(2), start(2, "pending", false), attempt(3))
			calls.ResumesWithPendingWrites = 1
			if finish == "finish" {
				commit(result(3, "pending", false, nil),
					&v1.Event{Payload: &v1.Event_RunCompleted{RunCompleted: &v1.RunCompleted{RunId: run, Attempt: 3}}})
				calls.WritesWithoutResult = 0
				check("steer", calls, session.RunCompleted, 3, 1)
			} else {
				commit(&v1.Event{Payload: &v1.Event_RunFailed{RunFailed: &v1.RunFailed{RunId: run, Attempt: 3}}})
				check("start", calls, session.RunFailed, 3, 1)
			}
		})
	}
}

func referenceInputStatus(t *testing.T, events []*v1.Event, inputID string) service.InputStatus {
	t.Helper()
	st, err := session.Reduce(events)
	if err != nil {
		t.Fatal(err)
	}
	out := service.InputStatus{AsOfSeq: st.Seq}
	ref := st.Input(inputID)
	if ref == nil {
		return out
	}
	run := st.Run(ref.RunID)
	out.Found, out.InputID, out.RunID = true, inputID, ref.RunID
	out.RunStatus, out.AttemptCount, out.TakeoverCount = run.Status, run.Attempt, uint32(run.Takeovers)
	for i, e := range events {
		if e.GetRunRequested().GetInputId() == inputID || e.GetSteered().GetInputId() == inputID || e.GetToolResult().GetInputId() == inputID {
			out.EventID, out.AcceptedSeq = e.GetId(), e.GetSeq()
			out.AcceptanceKind = "requested"
			if ref.Answered != "" {
				out.AcceptanceKind, out.AnsweredCallID = "answered", ref.Answered
			} else if ref.Steered {
				out.AcceptanceKind = "steered"
				suffix := referenceInputEvidence(events[i+1:], ref.RunID)
				out.SinceAcceptance = &suffix
			}
			break
		}
	}
	out.Calls = referenceInputEvidence(events, ref.RunID).Calls
	return out
}

func referenceInputEvidence(events []*v1.Event, runID string) service.InputAcceptanceEvidence {
	var out service.InputAcceptanceEvidence
	starts := map[string]uint64{}
	results := map[string]*v1.Event{}
	var resumes []uint64
	var suspend uint64
	for _, event := range events {
		if a := event.GetAttemptStarted(); a.GetRunId() == runID {
			if a.GetAttempt() > 1 {
				if suspend == 0 {
					out.AttemptsWithoutPriorSuspend++
				} else {
					resumes = append(resumes, event.GetSeq())
				}
			}
			suspend = 0
		}
		if event.GetRunSuspended().GetRunId() == runID {
			suspend = event.GetSeq()
		}
		if c := event.GetToolCallStarted(); c.GetRunId() == runID && !c.GetReadOnly() && starts[c.GetCallId()] == 0 {
			starts[c.GetCallId()] = event.GetSeq()
		}
		if r := event.GetToolResult(); r.GetRunId() == runID {
			results[r.GetCallId()] = event
		}
	}
	for _, seq := range resumes {
		for callID, start := range starts {
			if end := results[callID].GetSeq(); start < seq && (end == 0 || end > seq) {
				out.Calls.ResumesWithPendingWrites++
				break
			}
		}
	}
	for callID := range starts {
		result := results[callID].GetToolResult()
		if result == nil {
			out.Calls.WritesWithoutResult++
		}
		if result.GetIsError() {
			out.Calls.WritesWithError++
		}
		if result.GetBackgrounded() != nil {
			out.Calls.WritesBackgrounded++
		}
	}
	return out
}

func TestInputStatusScanBudget(t *testing.T) {
	s, _, sid := scheduleService(t, "UTC")
	ctx := t.Context()
	sub, err := s.SubmitWithID(ctx, sid, "first", model.TextBlocks("开始"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Load(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	events := make([]*v1.Event, 4096)
	for i := range events {
		events[i] = &v1.Event{Payload: &v1.Event_Steered{Steered: &v1.Steered{RunId: sub.RunID}}}
	}
	if err := s.Store.Commit(ctx, st, events...); err != nil {
		t.Fatal(err)
	}
	s.Store.Log = inputStatusReadHook{Log: s.Store.Log, beforePage: func() { t.Fatal("scan started despite exceeding budget") }}
	if got, err := s.InputStatus(ctx, sid, "first"); !errors.Is(err, service.ErrScanBudgetExceeded) || got.Found {
		t.Fatalf("over budget: %+v, %v", got, err)
	}
}

func TestInputStatusScriptReplay(t *testing.T) {
	for _, command := range []string{"echo", "call clock_now", "ask 什么时候？"} {
		t.Run(command, func(t *testing.T) {
			ctx := t.Context()
			clk := clock.Real{}
			agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "script/any",
				Capabilities: []string{"clock_now", "ask_user"}})
			if err != nil {
				t.Fatal(err)
			}
			gateway := model.NewGateway()
			gateway.Register("script", script.Provider{})
			core := app.NewCore(app.Memory(clk), app.Options{Clock: clk, Agents: agents, Model: gateway,
				Blobs: artifact.NewMemBlobs(), NodeAuth: node.InsecureDevAuth{}})
			s := core.Service
			sid, err := s.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SubmitWithID(ctx, sid, "start", model.TextBlocks(command)); err != nil {
				t.Fatal(err)
			}
			w := runtime.New(core.WorkerConfig("worker", workqueue.Pool{}, nil))
			drain := func() {
				t.Helper()
				for range 25 {
					work, err := w.Step(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if !work {
						return
					}
				}
				t.Fatal("script did not finish or suspend")
			}
			ids := []string{"start", "missing"}
			drain()
			if command == "ask 什么时候？" {
				if res, err := s.SubmitWithID(ctx, sid, "answer", model.TextBlocks("现在")); err != nil || res.Answered == "" {
					t.Fatalf("script question not answered: %+v, %v", res, err)
				}
				ids = append(ids, "answer")
				drain()
			}
			for _, id := range ids {
				got, err := s.InputStatus(ctx, sid, id)
				if err != nil {
					t.Fatal(err)
				}
				events, err := core.Store.Log.Read(ctx, sid, 0, int(got.AsOfSeq))
				if err != nil {
					t.Fatal(err)
				}
				if want := referenceInputStatus(t, events, id); !reflect.DeepEqual(got, want) {
					t.Fatalf("input %s: query %+v, reference %+v", id, got, want)
				}
			}
		})
	}
}
