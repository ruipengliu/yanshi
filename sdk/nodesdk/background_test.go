package nodesdk

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
)

func TestBackgroundCalls(t *testing.T) {
	ev := func(p any) *v1.Event {
		switch p := p.(type) {
		case *v1.AssistantMessage:
			return &v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: p}}
		case *v1.ToolResult:
			return &v1.Event{Payload: &v1.Event_ToolResult{ToolResult: p}}
		case *v1.RunRequested:
			return &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: p}}
		case *v1.Steered:
			return &v1.Event{Payload: &v1.Event_Steered{Steered: p}}
		case *v1.BackgroundCancelled:
			return &v1.Event{Payload: &v1.Event_BackgroundCancelled{BackgroundCancelled: p}}
		}
		panic(p)
	}
	bg := func(call string, at int64) *v1.ToolResult {
		return &v1.ToolResult{RunId: "r1", Attempt: 1, CallId: call, Backgrounded: &v1.Backgrounded{NodeId: "phone", Deadline: timestamppb.New(time.Unix(at, 0))}}
	}
	settle := func(call string) []*v1.ContentBlock {
		return []*v1.ContentBlock{{Kind: &v1.ContentBlock_BackgroundResult{BackgroundResult: &v1.BackgroundResult{CallId: call, Reason: "result"}}}}
	}
	var b BackgroundCalls
	for _, e := range []*v1.Event{
		ev(&v1.AssistantMessage{RunId: "r1", ToolCalls: []*v1.ToolCall{{CallId: "a", Capability: "phone__x"}, {CallId: "b", Capability: "phone__y"},
			{CallId: "c", Capability: "phone__z"}, {CallId: "d", Capability: "clock_now"}}}),
		ev(bg("a", 300)), ev(bg("b", 100)), ev(bg("c", 200)),
		ev(&v1.ToolResult{RunId: "r1", Attempt: 1, CallId: "d"}),
	} {
		b.OnEvent(e)
	}
	items := b.Items()
	if len(items) != 3 || items[0].CallID != "b" || items[0].Capability != "phone__y" || items[2].CallID != "a" || items[0].NodeID != "phone" {
		t.Fatalf("items = %+v", items)
	}
	b.OnEvent(ev(&v1.RunRequested{RunId: "r2", Input: settle("a")}))
	b.OnEvent(ev(&v1.Steered{RunId: "r2", Input: settle("b")}))
	b.OnEvent(ev(&v1.BackgroundCancelled{CallId: "c"}))
	if items := b.Items(); len(items) != 0 {
		t.Fatalf("after settling: %+v", items)
	}
	if r := BackgroundResultOf(ev(&v1.Steered{RunId: "r2", Input: settle("b")})); r.GetCallId() != "b" {
		t.Fatalf("BackgroundResultOf = %v", r)
	}
	if BackgroundResultOf(ev(&v1.Steered{RunId: "r2"})) != nil {
		t.Fatal("plain input has a background result")
	}
}
