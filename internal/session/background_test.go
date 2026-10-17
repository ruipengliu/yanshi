package session

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
)

func backgrounded(run string, n uint32, call, node string) *v1.Event {
	return &v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{RunId: run, Attempt: n, CallId: call,
		Backgrounded: &v1.Backgrounded{NodeId: node, Deadline: timestamppb.New(time.Unix(1000, 0))}}}}
}

func bgBlock(call, reason string, content ...*v1.ContentBlock) *v1.ContentBlock {
	return &v1.ContentBlock{Kind: &v1.ContentBlock_BackgroundResult{BackgroundResult: &v1.BackgroundResult{
		CallId: call, Capability: "x", Content: content, Reason: reason}}}
}

func bgRequested(run string, blocks ...*v1.ContentBlock) *v1.Event {
	return &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: run, Input: blocks}}}
}

func bgSteered(run string, blocks ...*v1.ContentBlock) *v1.Event {
	return &v1.Event{Payload: &v1.Event_Steered{Steered: &v1.Steered{RunId: run, Input: blocks}}}
}

func bgCancelled(call string) *v1.Event {
	return &v1.Event{Payload: &v1.Event_BackgroundCancelled{BackgroundCancelled: &v1.BackgroundCancelled{CallId: call, Capability: "x"}}}
}

func textBlock(s string) *v1.ContentBlock {
	return &v1.ContentBlock{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: s}}}
}

// bgPrefix：一个 Run 的三个设备调用都转入后台，Run 照常完成。
func bgPrefix(tail ...*v1.Event) []*v1.Event {
	return append([]*v1.Event{
		created(), requested("r1"), attempt("r1", 1),
		assistant("r1", 1, "c1", "c2", "c3"),
		routed("r1", 1, "c1", "phone"), backgrounded("r1", 1, "c1", "phone"),
		routed("r1", 1, "c2", "phone"), backgrounded("r1", 1, "c2", "phone"),
		routed("r1", 1, "c3", "phone"), backgrounded("r1", 1, "c3", "phone"),
		assistant("r1", 1), completed("r1", 1),
	}, tail...)
}

// 转入后台的调用在 Run 中完成、Run 继续；结果作为输入开启新 Run（记下来源），到期与取消同样了结它。
func TestBackgroundCallLifecycle(t *testing.T) {
	st, err := Reduce(build(bgPrefix()...))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(st.OpenBackground()); n != 3 {
		t.Fatalf("open background = %d", n)
	}
	if c := st.Run("r1").Calls[0]; !c.Done || !c.Background {
		t.Fatalf("call = %+v", c)
	}
	if d, ok := st.NextBackgroundDeadline(); !ok || !d.Equal(time.Unix(1000, 0)) {
		t.Fatalf("next deadline = %v %v", d, ok)
	}
	st, err = Reduce(build(bgPrefix(
		bgRequested("r2", bgBlock("c1", BackgroundReasonResult, textBlock("ok"))),
		attempt("r2", 1),
		bgSteered("r2", bgBlock("c2", BackgroundReasonTimeout)),
		bgCancelled("c3"),
	)...))
	if err != nil {
		t.Fatal(err)
	}
	if r := st.Run("r2"); r == nil || r.FromBackground != "c1" {
		t.Fatalf("r2 = %+v", r)
	}
	if len(st.Background) != 0 {
		t.Fatalf("background after settling = %v", st.Background)
	}
}

func TestBackgroundInvalid(t *testing.T) {
	base := func(tail ...*v1.Event) []*v1.Event {
		return append([]*v1.Event{created(), requested("r1"), attempt("r1", 1), assistant("r1", 1, "c1", "c2")}, tail...)
	}
	afterBg := func(tail ...*v1.Event) []*v1.Event {
		return base(append([]*v1.Event{routed("r1", 1, "c1", "phone"), backgrounded("r1", 1, "c1", "phone")}, tail...)...)
	}
	cases := map[string]struct {
		events []*v1.Event
		want   string
	}{
		"not dispatched":          {base(backgrounded("r1", 1, "c1", "phone")), "not dispatched"},
		"external background":     {base(routed("r1", 1, "c1", "phone"), backgrounded("r1", 0, "c1", "phone")), "external result cannot background"},
		"sandbox":                 {base(routed("r1", 1, "c1", "sbx_s"), backgrounded("r1", 1, "c1", "sbx_s")), "cannot run in the background"},
		"user":                    {base(routed("r1", 1, "c1", "@user"), backgrounded("r1", 1, "c1", "@user")), "cannot run in the background"},
		"wrong node":              {base(routed("r1", 1, "c1", "phone"), backgrounded("r1", 1, "c1", "pc")), "not dispatched"},
		"result after background": {afterBg(result("r1", 0, "c1")), "already done"},
		"settle twice": {afterBg(routed("r1", 1, "c2", "phone"), result("r1", 0, "c2"), assistant("r1", 1), completed("r1", 1),
			bgRequested("r2", bgBlock("c1", BackgroundReasonResult)), bgSteered("r2", bgBlock("c1", BackgroundReasonTimeout))), "not an open background call"},
		"cancel after result": {afterBg(bgSteered("r1", bgBlock("c1", BackgroundReasonResult)), bgCancelled("c1")), "not an open background call"},
		"unknown call":        {base(bgSteered("r1", bgBlock("c9", BackgroundReasonResult))), "not an open background call"},
		"mixed with text":     {afterBg(bgSteered("r1", textBlock("hi"), bgBlock("c1", BackgroundReasonResult))), "only block"},
		"with input id": {afterBg(&v1.Event{Payload: &v1.Event_Steered{Steered: &v1.Steered{RunId: "r1", InputId: "i",
			Input: []*v1.ContentBlock{bgBlock("c1", BackgroundReasonResult)}}}}), "input id"},
		"bad reason":        {afterBg(bgSteered("r1", bgBlock("c1", "later"))), "reason"},
		"timeout with data": {afterBg(bgSteered("r1", bgBlock("c1", BackgroundReasonTimeout, textBlock("x")))), "timed out with content"},
		"card in result": {afterBg(bgSteered("r1", bgBlock("c1", BackgroundReasonResult,
			&v1.ContentBlock{Kind: &v1.ContentBlock_Card{Card: &v1.Card{CardId: "x"}}}))), "platform block"},
		"nested": {afterBg(bgSteered("r1", bgBlock("c1", BackgroundReasonResult, bgBlock("c1", BackgroundReasonResult)))), "platform block"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Reduce(build(c.events...))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
		})
	}
}

// 委托派生的子 Session 不转后台：子 Run 结束后迟到的结果回不到父 Run（ADR-0035）。
func TestDelegatedSessionDoesNotBackground(t *testing.T) {
	child := &v1.Event{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{BusinessLine: "bl", EndUser: "u",
		Parent: &v1.ParentRef{SessionId: "p", RunId: "pr", CallId: "pc"}}}}
	_, err := Reduce(build(child, requested("r1"), attempt("r1", 1), assistant("r1", 1, "c1"),
		routed("r1", 1, "c1", "phone"), backgrounded("r1", 1, "c1", "phone")))
	if err == nil || !strings.Contains(err.Error(), "delegated session") {
		t.Fatalf("err = %v", err)
	}
}

// 关闭之后未了结的后台调用留在投影中（关闭方据此撤回投递），但不再算作未了结，也不能再了结。
func TestBackgroundAfterClose(t *testing.T) {
	st, err := Reduce(build(bgPrefix(closed())...))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Background) != 3 || len(st.OpenBackground()) != 0 {
		t.Fatalf("background = %d, open = %d", len(st.Background), len(st.OpenBackground()))
	}
	if _, ok := st.NextBackgroundDeadline(); ok {
		t.Fatal("closed session has a background deadline")
	}
}
