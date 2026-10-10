package runtime

import (
	"fmt"
	"strings"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

type logBuilder struct{ events []*v1.Event }

func (b *logBuilder) add(p any) *logBuilder {
	e := &v1.Event{SessionId: "s", Seq: uint64(len(b.events) + 1)}
	switch p := p.(type) {
	case *v1.SessionCreated:
		e.Payload = &v1.Event_SessionCreated{SessionCreated: p}
	case *v1.RunRequested:
		e.Payload = &v1.Event_RunRequested{RunRequested: p}
	case *v1.Steered:
		e.Payload = &v1.Event_Steered{Steered: p}
	case *v1.AttemptStarted:
		e.Payload = &v1.Event_AttemptStarted{AttemptStarted: p}
	case *v1.AssistantMessage:
		e.Payload = &v1.Event_AssistantMessage{AssistantMessage: p}
	case *v1.ToolResult:
		e.Payload = &v1.Event_ToolResult{ToolResult: p}
	case *v1.RunInterrupted:
		e.Payload = &v1.Event_RunInterrupted{RunInterrupted: p}
	case *v1.ContextCompacted:
		e.Payload = &v1.Event_ContextCompacted{ContextCompacted: p}
	case *v1.MemoryRecalled:
		e.Payload = &v1.Event_MemoryRecalled{MemoryRecalled: p}
	case *v1.RunCompleted:
		e.Payload = &v1.Event_RunCompleted{RunCompleted: p}
	}
	b.events = append(b.events, e)
	return b
}

func shape(msgs []model.Message) string {
	var parts []string
	for _, m := range msgs {
		switch m.Role {
		case model.RoleUser:
			parts = append(parts, "user:"+model.Text(m.Content))
		case model.RoleAssistant:
			ids := []string{}
			for _, c := range m.ToolCalls {
				ids = append(ids, c.GetCallId())
			}
			parts = append(parts, fmt.Sprintf("assistant%v", ids))
		case model.RoleTool:
			parts = append(parts, "tool:"+m.ToolCallID)
		}
	}
	return strings.Join(parts, " ")
}

func calls(ids ...string) []*v1.ToolCall {
	var out []*v1.ToolCall
	for _, id := range ids {
		out = append(out, &v1.ToolCall{CallId: id, Capability: "x"})
	}
	return out
}

func TestTranscriptDefersSteerUntilToolResults(t *testing.T) {
	b := (&logBuilder{}).
		add(&v1.SessionCreated{}).
		add(&v1.RunRequested{RunId: "r1", Input: model.TextBlocks("a")}).
		add(&v1.AttemptStarted{RunId: "r1", Attempt: 1}).
		add(&v1.AssistantMessage{RunId: "r1", Attempt: 1, ToolCalls: calls("c1", "c2")}).
		add(&v1.ToolResult{RunId: "r1", Attempt: 1, CallId: "c1"}).
		add(&v1.Steered{RunId: "r1", Input: model.TextBlocks("b")}).
		add(&v1.ToolResult{RunId: "r1", Attempt: 1, CallId: "c2"})
	st, err := session.Reduce(b.events)
	if err != nil {
		t.Fatal(err)
	}
	got := shape(Transcript(st, 0))
	want := "user:a assistant[c1 c2] tool:c1 tool:c2 user:b"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestTranscriptClosesCallsOfInterruptedRun(t *testing.T) {
	b := (&logBuilder{}).
		add(&v1.SessionCreated{}).
		add(&v1.RunRequested{RunId: "r1", Input: model.TextBlocks("a")}).
		add(&v1.AttemptStarted{RunId: "r1", Attempt: 1}).
		add(&v1.AssistantMessage{RunId: "r1", Attempt: 1, ToolCalls: calls("c1")}).
		add(&v1.RunInterrupted{RunId: "r1"}).
		add(&v1.RunRequested{RunId: "r2", Input: model.TextBlocks("b")})
	st, err := session.Reduce(b.events)
	if err != nil {
		t.Fatal(err)
	}
	msgs := Transcript(st, 0)
	if got, want := shape(msgs), "user:a assistant[c1] tool:c1 user:b"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if !strings.Contains(model.Text(msgs[2].Content), "not executed") {
		t.Fatalf("synthesized result = %q", model.Text(msgs[2].Content))
	}
}

// TestRecallSitsBeforeTheCurrentRunInput：召回只呈现当前 Run 的，位置在它的用户输入之前（之前 Run 的召回不再出现）；
// 系统指令因此不随召回变化，前缀缓存可以命中。
func TestRecallSitsBeforeTheCurrentRunInput(t *testing.T) {
	mem := func(c string) []*v1.RecalledMemory {
		return []*v1.RecalledMemory{{Id: "m", BusinessLine: "bl", Category: "preference", Content: c}}
	}
	b := (&logBuilder{}).
		add(&v1.SessionCreated{BusinessLine: "bl"}).
		add(&v1.RunRequested{RunId: "r1", Input: model.TextBlocks("a")}).
		add(&v1.AttemptStarted{RunId: "r1", Attempt: 1}).
		add(&v1.MemoryRecalled{RunId: "r1", Attempt: 1, Items: mem("旧")}).
		add(&v1.AssistantMessage{RunId: "r1", Attempt: 1, Content: model.TextBlocks("ok")}).
		add(&v1.RunCompleted{RunId: "r1", Attempt: 1}).
		add(&v1.RunRequested{RunId: "r2", Input: model.TextBlocks("b")}).
		add(&v1.AttemptStarted{RunId: "r2", Attempt: 1}).
		add(&v1.MemoryRecalled{RunId: "r2", Attempt: 1, Items: mem("新")}).
		add(&v1.AssistantMessage{RunId: "r2", Attempt: 1, ToolCalls: calls("c1")}).
		add(&v1.ToolResult{RunId: "r2", Attempt: 1, CallId: "c1"})
	st, err := session.Reduce(b.events)
	if err != nil {
		t.Fatal(err)
	}
	got := shape(Transcript(st, 0))
	if strings.Contains(got, "旧") || !strings.Contains(got, "新") {
		t.Fatalf("only the current run's recall belongs in the transcript: %s", got)
	}
	if i, j := strings.Index(got, "新"), strings.Index(got, "user:b"); i < 0 || j < i || strings.Index(got, "user:a") > i {
		t.Fatalf("recall not placed right before the current run's input: %s", got)
	}

	// 当前 Run 的输入被压缩进摘要后，召回紧跟摘要。
	b.add(&v1.ContextCompacted{RunId: "r2", Attempt: 1, ThroughSeq: 11, Summary: model.TextBlocks("S")})
	st, err = session.Reduce(b.events)
	if err != nil {
		t.Fatal(err)
	}
	msgs := Transcript(st, 0)
	if len(msgs) < 2 || !strings.Contains(model.Text(msgs[0].Content), "S") || !strings.Contains(model.Text(msgs[1].Content), "新") {
		t.Fatalf("recall not right after the summary: %s", shape(msgs))
	}
}
