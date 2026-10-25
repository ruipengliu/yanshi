package session

import (
	"testing"

	v1 "yanshi/gen/yanshi/v1"
)

func textBlocks(s string) []*v1.ContentBlock {
	return []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: s}}}}
}

// 来源由平台从事件推导（ADR-0039）：每种事件与内容块各得到哪种来源，闸门与判断据此判断。
func TestPieces(t *testing.T) {
	seg := func(sp v1.Speaker, key, text string, redacted bool) *v1.ContentBlock {
		return &v1.ContentBlock{Kind: &v1.ContentBlock_Capture{Capture: &v1.CaptureSegment{Speaker: sp, SpeakerKey: key, Text: text, Redacted: redacted}}}
	}
	ui := &v1.ContentBlock{Kind: &v1.ContentBlock_UiContext{UiContext: &v1.UIContext{Screen: "网页", Content: "第三方"}}}
	deviceOnly := &v1.ContentBlock{Kind: &v1.ContentBlock_UiContext{UiContext: &v1.UIContext{Device: "phone"}}}
	calls := func(n uint32, caps ...string) *v1.Event {
		m := &v1.AssistantMessage{RunId: "r1", Attempt: n}
		for i, c := range caps {
			m.ToolCalls = append(m.ToolCalls, &v1.ToolCall{CallId: string(rune('a' + i)), Capability: c})
		}
		return &v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: m}}
	}
	res := func(call string, content []*v1.ContentBlock, mod func(*v1.ToolResult)) *v1.Event {
		r := &v1.ToolResult{RunId: "r1", Attempt: 1, CallId: call, Content: content}
		if mod != nil {
			mod(r)
		}
		return &v1.Event{Payload: &v1.Event_ToolResult{ToolResult: r}}
	}
	st := &State{Created: &v1.SessionCreated{}, History: []*v1.Event{
		{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r1", Input: append(textBlocks("我不吃香菜"), ui, deviceOnly)}}},
		{Payload: &v1.Event_Steered{Steered: &v1.Steered{RunId: "r1", Input: textBlocks("转述的任务"), FromCall: "vc1"}}},
		{Payload: &v1.Event_CallTranscript{CallTranscript: &v1.CallTranscript{Role: "user", Text: "通话里说的"}}},
		{Payload: &v1.Event_CallTranscript{CallTranscript: &v1.CallTranscript{Role: "assistant", Text: "助手说的"}}},
		calls(1, "pc__read", "ask_user", "ask_user", "captures", "memory_search", "pc__send"),
		res("a", textBlocks("文件内容"), nil),
		res("b", textBlocks("打字回答"), nil),
		res("c", textBlocks("转述的回答"), func(r *v1.ToolResult) { r.FromCall = "vc1" }),
		res("d", []*v1.ContentBlock{textBlocks("【交谈】")[0], seg(v1.Speaker_SPEAKER_WEARER, "", "明天发报价", false),
			seg(v1.Speaker_SPEAKER_OTHER, "other-1", "好的", false), seg(v1.Speaker_SPEAKER_OTHER, "other-2", "", true)}, nil),
		res("e", textBlocks("平台的检索结果"), nil),
		res("f", textBlocks("占位"), func(r *v1.ToolResult) { r.Backgrounded = &v1.Backgrounded{NodeId: "n1"} }),
		{Payload: &v1.Event_Steered{Steered: &v1.Steered{RunId: "r1", Input: []*v1.ContentBlock{{Kind: &v1.ContentBlock_BackgroundResult{
			BackgroundResult: &v1.BackgroundResult{CallId: "f", Content: textBlocks("设备结果"), Reason: BackgroundReasonResult}}}}}}},
	}}
	type want struct {
		from   Provenance
		text   string
		result string // 调用结果的 call_id
	}
	wants := []want{
		{FromUser, "我不吃香菜", ""},
		{FromExternal, "网页\n\n第三方", ""},
		{FromRelay, "转述的任务", ""},
		{FromUser, "通话里说的", ""},
		{FromExternal, "文件内容", "a"},
		{FromUser, "打字回答", ""},
		{FromRelay, "转述的回答", ""},
		{FromUser, "明天发报价", ""},
		{FromOther, "好的", "d"},
		{FromExternal, "设备结果", "f"},
	}
	got := st.Pieces()
	if len(got) != len(wants) {
		t.Fatalf("pieces = %+v", got)
	}
	for i, w := range wants {
		p := got[i]
		if p.From != w.from || p.Text != w.text || p.Result.GetCallId() != w.result {
			t.Errorf("piece %d = {%v %q %q}, want %+v", i, p.From, p.Text, p.Result.GetCallId(), w)
		}
	}
}

// 委托派生的子 Session：委托的任务由父 Run 的模型写成，是转述；子 Session 中用户之后的输入仍是本人的话。
func TestPiecesDelegatedTask(t *testing.T) {
	st := &State{Created: &v1.SessionCreated{Parent: &v1.ParentRef{CallId: "pc1"}}, History: []*v1.Event{
		{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r1", InputId: DelegatedInputID("pc1"), Input: textBlocks("任务")}}},
		{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r2", Input: textBlocks("用户补充")}}},
	}}
	got := st.Pieces()
	if len(got) != 2 || got[0].From != FromRelay || got[1].From != FromUser {
		t.Fatalf("pieces = %+v", got)
	}
}
