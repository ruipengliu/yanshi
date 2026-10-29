package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

func redoTranscriptState(t *testing.T) *session.State {
	t.Helper()
	b := (&logBuilder{}).
		add(&v1.SessionCreated{BusinessLine: "bl"}).
		add(&v1.RunRequested{RunId: "prefix", Input: model.TextBlocks("earlier question")}).
		add(&v1.AttemptStarted{RunId: "prefix", Attempt: 1}).
		add(&v1.AssistantMessage{RunId: "prefix", Attempt: 1, Content: model.TextBlocks("earlier reply")}).
		add(&v1.RunCompleted{RunId: "prefix", Attempt: 1})
	for i := 0; i < session.KeepRuns+4; i++ {
		id := fmt.Sprintf("redo-%d", i)
		request := &v1.RunRequested{RunId: id, Input: model.TextBlocks("same question")}
		if i > 0 {
			request.RedoOf = fmt.Sprintf("redo-%d", i-1)
		}
		b.add(request).add(&v1.AttemptStarted{RunId: id, Attempt: 1})
		b.add(&v1.MemoryRecalled{RunId: id, Attempt: 1, Items: []*v1.RecalledMemory{{Content: "hidden-recall-" + id}}})
		if i == session.KeepRuns+3 {
			break
		}
		callID := "hidden-call-" + id
		b.add(&v1.AssistantMessage{RunId: id, Attempt: 1, Content: model.TextBlocks("hidden-assistant-" + id),
			ToolCalls: []*v1.ToolCall{{CallId: callID, Capability: "mcp_search__read", ArgumentsJson: `{"q":"hidden-argument"}`}},
			Usage:     &v1.Usage{InputTokens: 1000000, OutputTokens: 1000000}}).
			add(&v1.ToolCallStarted{RunId: id, Attempt: 1, CallId: callID, ReadOnly: true}).
			add(&v1.ToolResult{RunId: id, Attempt: 1, CallId: callID, Content: model.TextBlocks("hidden-result-poison@example.com")}).
			add(&v1.AssistantMessage{RunId: id, Attempt: 1, Content: model.TextBlocks("hidden-final-" + id), Usage: &v1.Usage{InputTokens: 1000000}}).
			add(&v1.RunCompleted{RunId: id, Attempt: 1})
	}
	st, err := session.Reduce(b.events)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func redoMessagesText(messages []model.Message) string {
	var out strings.Builder
	for _, m := range messages {
		out.WriteString(model.Text(m.Content))
		out.WriteString(m.ToolCallID)
		for _, c := range m.ToolCalls {
			out.WriteString(c.GetCallId() + c.GetArgumentsJson())
		}
	}
	return out.String()
}

func TestRedoAllModelViewsAndEstimation(t *testing.T) {
	st := redoTranscriptState(t)
	last := st.Active().ID
	for _, scope := range []string{"session", "run"} {
		t.Run(scope, func(t *testing.T) {
			def := &agentdef.Def{Context: agentdef.Context{Scope: scope, Window: 10000, CompactAt: 0.75, KeepRecent: 1}}
			view := contextView(st, def)
			messages := Transcript(view, 0)
			text := redoMessagesText(messages)
			for _, hidden := range []string{"hidden-assistant", "hidden-call", "hidden-argument", "hidden-result", "hidden-final"} {
				if strings.Contains(text, hidden) {
					t.Fatalf("leaked %s", hidden)
				}
			}
			if strings.Count(text, "same question") != 1 || !strings.Contains(text, "hidden-recall-"+last) {
				t.Fatal("redo must have one input and its fresh recall")
			}
			for i := 0; i < session.KeepRuns+3; i++ {
				if strings.Contains(text, fmt.Sprintf("hidden-recall-redo-%d", i)) {
					t.Fatal("old recall leaked")
				}
			}
			req := &model.Request{Messages: messages}
			if got := estimate(view, req, 0); got != model.EstimateRequest(req) {
				t.Fatalf("old usage calibrated estimate: %d", got)
			}
		})
	}
	// 截点在重答事件之前，也须知道完整投影中的替代关系。
	through := st.Active().RequestedSeq - 1
	prefix := transcriptThrough(st, 0, through)
	if text := redoMessagesText(prefix); strings.Contains(text, "hidden-") || strings.Contains(text, "same question") || !strings.Contains(text, "earlier reply") {
		t.Fatalf("old answer in prefix: %s", text)
	}
	cut, _ := planCompaction(st, &model.Request{Messages: Transcript(st, 0)}, agentdef.Context{Window: 10000, CompactAt: 0.75, KeepRecent: 1}, true)
	if cut == 0 {
		t.Fatal("expected an earlier prefix cut")
	}
	for _, e := range st.History {
		if e.GetSeq() == cut && (e.GetRunRequested().GetRunId() != "prefix" && e.GetAssistantMessage().GetRunId() != "prefix") {
			t.Fatal("compression selected superseded history")
		}
	}
	// 模型视图的获取不改安全历史，外部读取不能因重答而洗成用户依据。
	call := &v1.ToolCall{Capability: "memory_save", ArgumentsJson: `{"content":"联系 poison@example.com"}`}
	if _, gated := memoryGate(st, call); !gated {
		t.Fatal("redo bypassed memory approval after external MCP content")
	}
	// 重答之后再压缩也不能洗掉外部来源；既有闸门把摘要视为不能单独授权的依据。
	through = 0
	for _, e := range st.History {
		if e.GetSeq() < st.Active().RequestedSeq {
			through = e.GetSeq()
		}
	}
	if err := st.Apply(&v1.Event{SessionId: st.SessionID, Seq: st.Seq + 1, Payload: &v1.Event_ContextCompacted{ContextCompacted: &v1.ContextCompacted{
		RunId: last, Attempt: 1, ThroughSeq: through, Summary: model.TextBlocks("摘要"),
	}}}); err != nil {
		t.Fatal(err)
	}
	if _, gated := memoryGate(st, call); !gated {
		t.Fatal("compaction after redo bypassed memory approval")
	}
}

func TestRedoSummaryPrefixAndTextFallback(t *testing.T) {
	st := redoTranscriptState(t)
	through := st.Active().RequestedSeq - 1
	var history []*v1.Event
	for _, e := range st.History {
		if e.GetSeq() <= through {
			history = append(history, e)
		}
	}
	in := SummaryInput{History: history, SupersededRuns: st.SupersededRuns, TargetTokens: 100,
		Prefix: &model.Request{System: "stable system", Tools: []model.ToolSpec{{Name: "read"}}, Messages: transcriptThrough(st, 0, through)}}
	for _, fallback := range []bool{false, true} {
		f := &fakeSummarizer{toolOnly: fallback}
		if _, err := (ModelCompactor{Model: f}).Summarize(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		want := 1
		if fallback {
			want = 2
		}
		if len(f.reqs) != want {
			t.Fatalf("requests = %d, want %d", len(f.reqs), want)
		}
		for _, req := range f.reqs {
			text := redoMessagesText(req.Messages)
			if strings.Contains(text, "hidden-") || strings.Contains(text, "same question") || !strings.Contains(text, "earlier reply") {
				t.Fatalf("summary leaked a replaced run: %s", text)
			}
		}
	}
}
