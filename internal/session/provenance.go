package session

import (
	"strings"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/askuser"
)

// Provenance 是一段内容的来源（ADR-0039，docs/design/m5-inputs-triggers-loops.md §3）：由谁产生、是不是对 Agent 说的。
// 它由平台从事件推导（Pieces），不由写入方填写：存储的来源字段可以被自报，推导只认平台写入的事件。唯一推导不出的
// 旁听说话人是结构化字段，只由 attested 的结果写入（Apply 拒绝其他结果中的旁听片段）。Memory 写入闸门与注入判断都只看它。
type Provenance uint8

const (
	// FromUser 是 EndUser 本人的话：输入、插话、对提问的回答、通话中用户说的话、旁听中佩戴者的话。可以作为写入 Memory 的依据。
	FromUser Provenance = iota + 1
	// FromOther 是旁听中他人的话（ADR-0036）：外部内容，需要注入判断。
	FromOther
	// FromExternal 是设备、沙箱、MCP、委托的调用结果，后台调用迟到的结果，以及可能含第三方内容的界面上下文。
	FromExternal
	// FromRelay 是别的模型写成、作为输入进入的内容：通话中语音模型的转述（docs/design/m3-call.md §6）、委托的任务
	// （docs/design/m5-multi-agent.md §8）。不是用户的原话，可能转述了外部内容。
	FromRelay
	// FromSchedule 是平台定时：不是用户依据，也不是供注入判断的外部内容。
	FromSchedule
)

// Piece 是上下文中一段带来源的内容。平台写的说明与 Agent 自己的输出不在其中；定时输入保留来源，
// 使写入闸门不会把创建方的文字当作用户在到点时亲口说的。
type Piece struct {
	From Provenance
	// Text 是这段内容的文字：依据比对（FromUser）与注入判断（调用结果）用它。旁听结果中他人的话合成一段。
	Text string
	// Result 非空表示这段内容来自一个调用结果（含后台调用迟到的结果，合成为 ToolResult）；注入判断只判断调用结果。
	Result *v1.ToolResult
}

// Pieces 按 History 的顺序给出上下文中带来源的内容，定时输入由事件标记推导为平台定时。
func (s *State) Pieces() []Piece {
	external, questions := map[string]bool{}, map[string]bool{}
	mark := func(tc *v1.ToolCall) {
		// 路由到设备、沙箱的能力、MCP 工具与委托的名称都含 "__"（"<label>__<能力>"、"mcp_<server>__<工具>"），进程内能力不含。
		if strings.Contains(tc.GetCapability(), "__") {
			external[tc.GetCallId()] = true
		}
		if tc.GetCapability() == askuser.Capability {
			questions[tc.GetCallId()] = true
		}
	}
	for _, r := range s.Runs {
		for _, c := range r.Calls {
			mark(c.Call)
		}
	}
	delegated := ""
	if p := s.Created.GetParent(); p != nil {
		delegated = DelegatedInputID(p.GetCallId())
	}
	var out []Piece
	input := func(runID string, blocks []*v1.ContentBlock, relay bool) {
		if t := blockText(blocks); t != "" {
			from := FromUser
			if relay {
				from = FromRelay
			}
			out = append(out, Piece{From: from, Text: t})
		}
		out = append(out, uiPieces(blocks)...)
		// 后台调用迟到的结果带着设备返回的内容（ADR-0035），与设备调用的结果一样是外部内容；到期没有内容。
		if b := BackgroundInput(blocks); b.GetReason() == BackgroundReasonResult {
			out = append(out, Piece{From: FromExternal, Text: blockText(b.GetContent()), Result: &v1.ToolResult{
				RunId: runID, CallId: b.GetCallId(), Content: b.GetContent(), IsError: b.GetIsError()}})
		}
	}
	for _, e := range s.History {
		switch p := e.GetPayload().(type) {
		case *v1.Event_RunRequested:
			m := p.RunRequested
			if m.GetSchedule() != nil {
				out = append(out, Piece{From: FromSchedule, Text: blockText(m.GetInput())})
			} else {
				input(m.GetRunId(), m.GetInput(), m.GetFromCall() != "" || (delegated != "" && m.GetInputId() == delegated))
			}
		case *v1.Event_Steered:
			m := p.Steered
			input(m.GetRunId(), m.GetInput(), m.GetFromCall() != "")
		case *v1.Event_CallTranscript:
			if p.CallTranscript.GetRole() == "user" {
				out = append(out, Piece{From: FromUser, Text: p.CallTranscript.GetText()})
			}
		case *v1.Event_AssistantMessage:
			for _, tc := range p.AssistantMessage.GetToolCalls() {
				mark(tc)
			}
		case *v1.Event_ToolResult:
			out = append(out, resultPieces(p.ToolResult, external[p.ToolResult.GetCallId()], questions[p.ToolResult.GetCallId()])...)
		}
	}
	return out
}

func resultPieces(m *v1.ToolResult, external, question bool) []Piece {
	var out []Piece
	switch {
	case m.GetBackgrounded() != nil:
		// 转入后台的占位结果由平台写成，不含设备的内容（ADR-0035）。
	case m.GetFromCall() != "":
		// 语音模型转述的回答（run_task 恰好回答了提问）。
		out = append(out, Piece{From: FromRelay, Text: blockText(m.GetContent())})
	case external && !m.GetAttested():
		out = append(out, Piece{From: FromExternal, Text: blockText(m.GetContent()), Result: m})
	case question && !m.GetIsError():
		if t := blockText(m.GetContent()); t != "" {
			out = append(out, Piece{From: FromUser, Text: t})
		}
	}
	// Apply 只接受 attested 结果中的旁听片段，来源按结构化说话人推导。
	// 文本中的前缀不能改变来源；拦截的片段没有内容。
	var others []string
	for _, c := range m.GetContent() {
		g := c.GetCapture()
		if g == nil || g.GetRedacted() {
			continue
		}
		switch g.GetSpeaker() {
		case v1.Speaker_SPEAKER_WEARER:
			out = append(out, Piece{From: FromUser, Text: g.GetText()})
		case v1.Speaker_SPEAKER_OTHER:
			others = append(others, g.GetText())
		}
	}
	if len(others) > 0 {
		out = append(out, Piece{From: FromOther, Text: strings.Join(others, "\n"), Result: m})
	}
	return append(out, uiPieces(m.GetContent())...)
}

// uiPieces 是输入或回答附带的界面上下文（可能含第三方内容，docs/design/m3-duplex-channel.md §8）。只注明了用户所在设备的
// 界面上下文由平台填写，不算外部内容。
func uiPieces(blocks []*v1.ContentBlock) []Piece {
	var out []Piece
	for _, b := range blocks {
		if u := b.GetUiContext(); u != nil && (u.GetScreen() != "" || u.GetRef() != "" || u.GetSelection() != "" || u.GetContent() != "") {
			out = append(out, Piece{From: FromExternal, Text: u.GetScreen() + "\n" + u.GetSelection() + "\n" + u.GetContent()})
		}
	}
	return out
}

// blockText 是内容块中文本块的拼接（与 model.Text 相同；session 不依赖 model）。
func blockText(blocks []*v1.ContentBlock) string {
	var b strings.Builder
	for _, c := range blocks {
		b.WriteString(c.GetText().GetText())
	}
	return b.String()
}
