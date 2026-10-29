package session

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
)

func redoRequested(run, original string) *v1.Event {
	e := requested(run)
	e.GetRunRequested().RedoOf = original
	return e
}

func TestRedoBlocker(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*State)
		want string
	}{
		{"plain", func(*State) {}, ""},
		{"closed", func(s *State) { s.Closed = &v1.SessionClosed{} }, "session_closed"},
		{"newer run", func(s *State) { s.Runs = append(s.Runs, &Run{ID: "new"}) }, "not_last_run"},
		{"failed", func(s *State) { s.Run("r").Status = RunFailed }, "not_completed"},
		{"interrupted", func(s *State) { s.Run("r").Status = RunInterrupted }, "not_completed"},
		{"queued", func(s *State) { s.Run("r").Status = RunQueued }, "not_completed"},
		{"running", func(s *State) { s.Run("r").Status = RunRunning }, "not_completed"},
		{"suspended", func(s *State) { s.Run("r").Status = RunSuspended }, "not_completed"},
		{"child", func(s *State) { s.Created.Parent = &v1.ParentRef{CallId: "parent"} }, "delegated_session"},
		{"schedule", func(s *State) { s.Run("r").Request.Schedule = &v1.ScheduleMark{} }, "platform_input"},
		{"call", func(s *State) { s.Run("r").Request.FromCall = "voice" }, "platform_input"},
		{"missing request", func(s *State) { s.Run("r").Request = nil }, "platform_input"},
		{"missing input seq", func(s *State) { s.Run("r").RequestedSeq = 0 }, "platform_input"},
		{"card input", func(s *State) {
			s.Run("r").Request.Input = []*v1.ContentBlock{{Kind: &v1.ContentBlock_CardAction{CardAction: &v1.CardAction{}}}}
		}, "card_action_input"},
		{"background input", func(s *State) {
			s.Run("r").Request.Input = []*v1.ContentBlock{{Kind: &v1.ContentBlock_BackgroundResult{BackgroundResult: &v1.BackgroundResult{}}}}
		}, "platform_input"},
		{"capture input", func(s *State) {
			s.Run("r").Request.Input = []*v1.ContentBlock{{Kind: &v1.ContentBlock_Capture{Capture: &v1.CaptureSegment{}}}}
		}, "platform_input"},
		{"card block input", func(s *State) {
			s.Run("r").Request.Input = []*v1.ContentBlock{{Kind: &v1.ContentBlock_Card{Card: &v1.Card{}}}}
		}, "platform_input"},
		{"steered", func(s *State) { s.Run("r").Steered = true }, "steered"},
		{"compacted after input", func(s *State) { s.CompactedAt = s.Run("r").RequestedSeq + 1 }, "compacted"},
		{"compacted before input", func(s *State) { s.CompactedAt = s.Run("r").RequestedSeq - 1 }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := Reduce(build(created(), requested("r"), attempt("r", 1), completed("r", 1)))
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(st)
			if got := st.RedoBlocker("r"); got != tc.want {
				t.Fatalf("blocker = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRedoCallEligibility(t *testing.T) {
	for _, tc := range []struct {
		name string
		call *Call
		want string
	}{
		{"read", &Call{Call: &v1.ToolCall{Capability: "clock_now"}, StartedAttempts: []uint32{1}, StartedReadOnly: []bool{true}}, ""},
		{"card", &Call{Call: &v1.ToolCall{Capability: ShowCardCapability}}, ""},
		{"no start", &Call{Call: &v1.ToolCall{Capability: "clock_now"}}, "not_read_only"},
		{"old log", &Call{Call: &v1.ToolCall{Capability: "clock_now"}, StartedAttempts: []uint32{1}, StartedReadOnly: []bool{false}}, "not_read_only"},
		{"missing evidence", &Call{Call: &v1.ToolCall{Capability: "clock_now"}, StartedAttempts: []uint32{1}}, "not_read_only"},
		{"read in every attempt", &Call{Call: &v1.ToolCall{Capability: "clock_now"}, StartedAttempts: []uint32{1, 2}, StartedReadOnly: []bool{true, true}}, ""},
		{"takeover write", &Call{Call: &v1.ToolCall{Capability: "clock_now"}, StartedAttempts: []uint32{1, 2}, StartedReadOnly: []bool{true, false}}, "not_read_only"},
		{"approval waiting", &Call{Call: &v1.ToolCall{Capability: ShowCardCapability}, Approval: &Approval{}}, "approval_requested"},
		{"approval denied", &Call{Call: &v1.ToolCall{Capability: ShowCardCapability}, Approval: &Approval{Decided: true}}, "approval_requested"},
		{"approval approved", &Call{Call: &v1.ToolCall{Capability: ShowCardCapability}, Approval: &Approval{Decided: true, Approved: true}}, "approval_requested"},
		{"approval timed out", &Call{Call: &v1.ToolCall{Capability: ShowCardCapability}, Approval: &Approval{}, Done: true}, "approval_requested"},
		{"ask", &Call{Call: &v1.ToolCall{Capability: "ask_user"}}, "ask_user"},
		{"delegation before start", &Call{Call: &v1.ToolCall{Capability: "agent__helper"}}, "delegation"},
		{"delegation dispatched", &Call{Call: &v1.ToolCall{}, NodeID: "@agent/child"}, "delegation"},
		{"background", &Call{Call: &v1.ToolCall{}, Background: true}, "background_call"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := Reduce(build(created(), requested("r"), attempt("r", 1), completed("r", 1)))
			if err != nil {
				t.Fatal(err)
			}
			st.Run("r").Calls = []*Call{tc.call}
			if got := st.RedoBlocker("r"); got != tc.want {
				t.Fatalf("blocker = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestApplyRedoRejectsAndPreservesState(t *testing.T) {
	first := requested("a")
	first.GetRunRequested().Input = []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: "original"}}}}
	st, err := Reduce(build(created(), first, attempt("a", 1), completed("a", 1)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*v1.Event)
	}{
		{"cross session", func(e *v1.Event) { e.SessionId = "other" }},
		{"unknown target", func(e *v1.Event) { e.GetRunRequested().RedoOf = "other-run" }},
		{"self cycle", func(e *v1.Event) { e.GetRunRequested().RedoOf = "b" }},
		{"duplicate run", func(e *v1.Event) { e.GetRunRequested().RunId = "a" }},
		{"changed input", func(e *v1.Event) { e.GetRunRequested().Input = nil }},
		{"schedule", func(e *v1.Event) { e.GetRunRequested().Schedule = &v1.ScheduleMark{} }},
		{"from call", func(e *v1.Event) { e.GetRunRequested().FromCall = "voice" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := redoRequested("b", "a")
			e.GetRunRequested().Input = first.GetRunRequested().GetInput()
			e.SessionId, e.Seq = st.SessionID, st.Seq+1
			tc.edit(e)
			before := st.Clone()
			if err := st.Apply(e); err == nil {
				t.Fatal("accepted invalid redo")
			}
			if !Equal(before, st) {
				t.Fatal("rejected redo changed state")
			}
		})
	}
}

func TestRedoChainSurvivesRunTrimmingAndSnapshot(t *testing.T) {
	events := []*v1.Event{created(), requested("r0"), attempt("r0", 1), completed("r0", 1)}
	const count = KeepRuns + 8
	for i := 1; i <= count; i++ {
		run := fmt.Sprintf("r%d", i)
		events = append(events, redoRequested(run, fmt.Sprintf("r%d", i-1)), attempt(run, 1), completed(run, 1))
	}
	st, err := Reduce(build(events...))
	if err != nil {
		t.Fatal(err)
	}
	if st.Run("r0") != nil || len(st.SupersededRuns) != count || st.RedoBlocker(fmt.Sprintf("r%d", count)) != "" {
		t.Fatal("chain or last run eligibility was lost")
	}
	for i := range count {
		if !st.SupersededRuns[fmt.Sprintf("r%d", i)] {
			t.Fatalf("missing replaced run %d", i)
		}
	}
	data, err := proto.Marshal(st.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	snap := &v1.SessionSnapshot{}
	if err := proto.Unmarshal(data, snap); err != nil {
		t.Fatal(err)
	}
	restored, err := FromSnapshot(snap)
	if err != nil || !Equal(st, restored) {
		t.Fatalf("snapshot: %v", err)
	}
	bad := redoRequested("again", "r0")
	bad.SessionId, bad.Seq = st.SessionID, st.Seq+1
	if err := restored.Apply(bad); err == nil || !strings.Contains(err.Error(), "not_last_run") {
		t.Fatalf("redo of trimmed run: %v", err)
	}
	last := fmt.Sprintf("r%d", count)
	tail := build(redoRequested("failed-redo", last), attempt("failed-redo", 1), failed("failed-redo", 1))
	for _, e := range tail {
		e.Seq = restored.Seq + 1
		if err := restored.Apply(e); err != nil {
			t.Fatal(err)
		}
	}
	if !restored.SupersededRuns[last] || restored.RedoBlocker("failed-redo") != "not_completed" || st.SupersededRuns[last] {
		t.Fatal("failure rolled back replacement or shared maps were mutated")
	}
}

func TestRedoFactsFromEvents(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprint(compact), func(t *testing.T) {
			events := []*v1.Event{created(), requested("r"), attempt("r", 1), steered("r")}
			if compact {
				events = append(events, &v1.Event{Payload: &v1.Event_ContextCompacted{ContextCompacted: &v1.ContextCompacted{RunId: "r", Attempt: 1, ThroughSeq: 2}}})
			}
			events = append(events, completed("r", 1))
			st, err := Reduce(build(events...))
			if err != nil {
				t.Fatal(err)
			}
			r := st.Run("r")
			if !r.Steered || r.RequestedSeq != 2 || r.Request == nil || st.RedoBlocker("r") == "" {
				t.Fatal("projection lost redo facts")
			}
			restored, err := FromSnapshot(st.Snapshot())
			if err != nil || !Equal(st, restored) || restored.RedoBlocker("r") != st.RedoBlocker("r") {
				t.Fatalf("snapshot lost eligibility: %v", err)
			}
		})
	}
}

func TestApplyRedoCardActionCarriers(t *testing.T) {
	show := assistant("a", 1, "card-call")
	show.GetAssistantMessage().ToolCalls[0].Capability = ShowCardCapability
	shown := result("a", 1, "card-call")
	shown.GetToolResult().Content = []*v1.ContentBlock{{Kind: &v1.ContentBlock_Card{Card: &v1.Card{CardId: "reused", CallId: "card-call"}}}}
	st, err := Reduce(build(created(), requested("a"), attempt("a", 1), show, started("a", 1, "card-call"), shown, completed("a", 1), redoRequested("b", "a")))
	if err != nil {
		t.Fatal(err)
	}
	if !st.SupersededCardCalls["card-call"] {
		t.Fatal("card call was not invalidated")
	}
	restored, err := FromSnapshot(st.Snapshot())
	if err != nil || !Equal(st, restored) || !restored.SupersededCardCalls["card-call"] {
		t.Fatalf("snapshot lost card invalidation: %v", err)
	}
	a := &v1.CardAction{CardId: "reused", CallId: "card-call", Action: "ok"}
	blocks := []*v1.ContentBlock{{Kind: &v1.ContentBlock_CardAction{CardAction: a}}}
	ask := assistant("b", 1, "question")
	ask.GetAssistantMessage().ToolCalls[0].Capability = "ask_user"
	start := started("b", 1, "question")
	start.GetToolCallStarted().NodeId = "@user"
	for _, e := range []*v1.Event{attempt("b", 1), ask, start} {
		e.SessionId, e.Seq = st.SessionID, st.Seq+1
		if err := st.Apply(e); err != nil {
			t.Fatal(err)
		}
	}
	for _, carrier := range []string{"steer", "answer field", "answer content", "answer both"} {
		t.Run(carrier, func(t *testing.T) {
			var e *v1.Event
			if carrier == "steer" {
				e = steered("b")
				e.GetSteered().Input = blocks
			} else {
				e = result("b", 0, "question")
				m := e.GetToolResult()
				m.InputId = "action-id"
				if carrier != "answer content" {
					m.CardAction = a
				}
				if carrier != "answer field" {
					m.Content = append([]*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: "回答"}}}}, blocks...)
				}
			}
			e.SessionId, e.Seq = st.SessionID, st.Seq+1
			if err := st.Apply(e); err == nil || !strings.Contains(err.Error(), "superseded_call") {
				t.Fatalf("accepted %s: %v", carrier, err)
			}
		})
	}
	end := interrupted("b")
	end.SessionId, end.Seq = st.SessionID, st.Seq+1
	if err := st.Apply(end); err != nil {
		t.Fatal(err)
	}
	request := requested("c")
	request.GetRunRequested().Input = blocks
	request.SessionId, request.Seq = st.SessionID, st.Seq+1
	if err := st.Apply(request); err == nil || !strings.Contains(err.Error(), "superseded_call") {
		t.Fatalf("accepted stale action in RunRequested: %v", err)
	}
	// 新卡片可复用 card_id，失效键只认旧 call_id。
	request.GetRunRequested().Input = []*v1.ContentBlock{{Kind: &v1.ContentBlock_CardAction{CardAction: &v1.CardAction{CardId: "reused", CallId: "new-call", Action: "ok"}}}}
	if err := st.Apply(request); err != nil {
		t.Fatalf("new card version rejected: %v", err)
	}
}

func TestRedoCardCallsSurviveRunTrimming(t *testing.T) {
	var events []*v1.Event
	events = append(events, created())
	const count = KeepRuns + 5
	for i := 0; i <= count; i++ {
		run := fmt.Sprintf("r%d", i)
		request := requested(run)
		if i > 0 {
			request.GetRunRequested().RedoOf = fmt.Sprintf("r%d", i-1)
		}
		call := fmt.Sprintf("card-%d", i)
		message := assistant(run, 1, call)
		message.GetAssistantMessage().ToolCalls[0].Capability = ShowCardCapability
		shown := result(run, 1, call)
		shown.GetToolResult().Content = []*v1.ContentBlock{{Kind: &v1.ContentBlock_Card{Card: &v1.Card{CardId: "same-card", CallId: call}}}}
		events = append(events, request, attempt(run, 1), message, started(run, 1, call), shown, completed(run, 1))
	}
	st, err := Reduce(build(events...))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := FromSnapshot(st.Snapshot())
	if err != nil || !Equal(st, restored) {
		t.Fatalf("snapshot: %v", err)
	}
	if st.Run("r0") != nil || len(restored.SupersededCardCalls) != count {
		t.Fatal("trimmed run lost its invalidated card call")
	}
	for i := 0; i <= count; i++ {
		if got := restored.SupersededCardCalls[fmt.Sprintf("card-%d", i)]; got != (i < count) {
			t.Fatalf("card %d invalidation = %v", i, got)
		}
	}
}

func TestInterruptedRedoDoesNotRestoreOriginal(t *testing.T) {
	st, err := Reduce(build(created(), requested("a"), attempt("a", 1), completed("a", 1), redoRequested("b", "a"), interrupted("b")))
	if err != nil {
		t.Fatal(err)
	}
	if !st.SupersededRuns["a"] || st.RedoBlocker("b") != "not_completed" || st.RedoBlocker("a") != "not_last_run" {
		t.Fatal("interruption restored the original run")
	}
}
