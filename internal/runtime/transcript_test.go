package runtime

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

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
	case *v1.ToolCallStarted:
		e.Payload = &v1.Event_ToolCallStarted{ToolCallStarted: p}
	case *v1.RunInterrupted:
		e.Payload = &v1.Event_RunInterrupted{RunInterrupted: p}
	case *v1.ContextCompacted:
		e.Payload = &v1.Event_ContextCompacted{ContextCompacted: p}
	case *v1.MemoryRecalled:
		e.Payload = &v1.Event_MemoryRecalled{MemoryRecalled: p}
	case *v1.RunCompleted:
		e.Payload = &v1.Event_RunCompleted{RunCompleted: p}
	case *v1.CallStarted:
		e.Payload = &v1.Event_CallStarted{CallStarted: p}
	case *v1.CallTranscript:
		e.Payload = &v1.Event_CallTranscript{CallTranscript: p}
	case *v1.BackgroundCancelled:
		e.Payload = &v1.Event_BackgroundCancelled{BackgroundCancelled: p}
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

// TestCallInTranscript：Call 中双方的话以用户消息进入上下文（调用尚无结果时顺延到结果之后）；
// Call 派生的任务附上"结果会被朗读"的说明。
func TestCallInTranscript(t *testing.T) {
	b := (&logBuilder{}).
		add(&v1.SessionCreated{}).
		add(&v1.CallStarted{CallId: "vc"}).
		add(&v1.CallTranscript{CallId: "vc", Role: "user", Text: "PPT 第三页写了什么"}).
		add(&v1.RunRequested{RunId: "r1", Input: model.TextBlocks("读取 PPT 第三页"), FromCall: "vc"}).
		add(&v1.AttemptStarted{RunId: "r1", Attempt: 1}).
		add(&v1.AssistantMessage{RunId: "r1", Attempt: 1, ToolCalls: calls("c1")}).
		add(&v1.CallTranscript{CallId: "vc", Role: "assistant", Text: "我去看看", Interrupted: true}).
		add(&v1.ToolResult{RunId: "r1", Attempt: 1, CallId: "c1"})
	st, err := session.Reduce(b.events)
	if err != nil {
		t.Fatal(err)
	}
	got := shape(Transcript(st, 0))
	want := "user:[语音通话] 用户：PPT 第三页写了什么 user:读取 PPT 第三页" + model.FromCallNote +
		" assistant[c1] tool:c1 user:[语音通话] 语音助手：我去看看……（被用户打断）"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// TestBackgroundInTranscript：转入后台的调用以占位结果交还模型；迟到的结果、到期与取消以带说明的用户消息出现在各自的位置，
// 调用进行中到达的推迟到结果之后（ADR-0035）。设备返回的内容与调用结果一样截断。
func TestBackgroundInTranscript(t *testing.T) {
	bg := func(call, reason string, content ...*v1.ContentBlock) []*v1.ContentBlock {
		return []*v1.ContentBlock{{Kind: &v1.ContentBlock_BackgroundResult{BackgroundResult: &v1.BackgroundResult{
			CallId: call, Capability: "phone__take_photo", Content: content, Reason: reason}}}}
	}
	backgrounded := func(run, call string) *v1.ToolResult {
		return &v1.ToolResult{RunId: run, Attempt: 1, CallId: call, Content: model.TextBlocks("queued"),
			Backgrounded: &v1.Backgrounded{NodeId: "phone", Deadline: timestamppb.Now()}}
	}
	routed := func(run, call string) *v1.ToolCallStarted {
		return &v1.ToolCallStarted{RunId: run, Attempt: 1, CallId: call, NodeId: "phone", Deadline: timestamppb.Now()}
	}
	long := strings.Repeat("长", 3000)
	photo := func(ids ...string) []*v1.ToolCall {
		out := calls(ids...)
		for _, c := range out {
			c.Capability = "phone__take_photo"
		}
		return out
	}
	b := (&logBuilder{}).
		add(&v1.SessionCreated{}).
		add(&v1.RunRequested{RunId: "r1", Input: model.TextBlocks("a")}).
		add(&v1.AttemptStarted{RunId: "r1", Attempt: 1}).
		add(&v1.AssistantMessage{RunId: "r1", Attempt: 1, ToolCalls: photo("c1", "c2", "c3")}).
		add(routed("r1", "c1")).add(backgrounded("r1", "c1")).
		add(routed("r1", "c2")).add(backgrounded("r1", "c2")).
		add(routed("r1", "c3")).add(backgrounded("r1", "c3")).
		add(&v1.AssistantMessage{RunId: "r1", Attempt: 1, Content: model.TextBlocks("queued")}).
		add(&v1.RunCompleted{RunId: "r1", Attempt: 1}).
		add(&v1.RunRequested{RunId: "r2", Input: bg("c1", session.BackgroundReasonResult, model.TextBlocks(long)...)}).
		add(&v1.AttemptStarted{RunId: "r2", Attempt: 1}).
		add(&v1.AssistantMessage{RunId: "r2", Attempt: 1, ToolCalls: calls("c4")}).
		add(&v1.BackgroundCancelled{CallId: "c2", Capability: "phone__take_photo"}).
		add(&v1.Steered{RunId: "r2", Input: bg("c3", session.BackgroundReasonTimeout)}).
		add(&v1.ToolResult{RunId: "r2", Attempt: 1, CallId: "c4"})
	st, err := session.Reduce(b.events)
	if err != nil {
		t.Fatal(err)
	}
	msgs := Transcript(st, 100)
	var roles []string
	for _, m := range msgs {
		roles = append(roles, string(m.Role))
	}
	got := strings.Join(roles, " ")
	want := "user assistant tool tool tool assistant user assistant tool user user"
	if got != want {
		t.Fatalf("roles %s, want %s", got, want)
	}
	result := model.Text(msgs[6].Content)
	if !strings.HasPrefix(result, "[后台调用结果]") || !strings.Contains(result, "phone__take_photo") || !strings.Contains(result, "c1") ||
		!strings.Contains(result, "不是用户说的话") || !strings.Contains(result, "已截断") || strings.Count(result, "长") > 400 {
		t.Fatalf("background result = %.300q", result)
	}
	if c := model.Text(msgs[9].Content); !strings.HasPrefix(c, "[后台调用已取消]") || !strings.Contains(c, "c2") {
		t.Fatalf("cancelled = %q", c)
	}
	if c := model.Text(msgs[10].Content); !strings.HasPrefix(c, "[后台调用到期]") || !strings.Contains(c, "c3") {
		t.Fatalf("timeout = %q", c)
	}
	// 估算与呈现一致：每个事件的估算等于它在 Transcript 中的消息。
	if n := eventTokens(b.events[12], 100); n != model.EstimateMessage(msgs[6]) {
		t.Fatalf("estimate %d, rendered %d", n, model.EstimateMessage(msgs[6]))
	}
}
