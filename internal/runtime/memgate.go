package runtime

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/askuser"
	"yanshi/internal/delegation"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

// Memory 写入闸门（docs/design/m4-memory-grant.md §3.1，ADR-0023）。
//
// 记忆投毒是持久化的提示注入：网页、文件、调用结果中的指令被写成 Memory 后，会在之后的每个 Session 中被召回。
// 闸门只收紧、不放宽：当上下文里出现过外部来源的内容（设备、沙箱、MCP 的调用结果，或可能概括了它们的压缩摘要），
// 而要写入的内容在用户本人的输入中找不到依据时，这次 memory_save 需要用户批准。判断只依据投影（日志），
// 不调用模型，因此是确定性的，重放与模拟都得到同一结论。

// memoryGateCapability 是受闸门约束的能力。
const memoryGateCapability = "memory_save"

var (
	emailRE  = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	urlRE    = regexp.MustCompile(`https?://\S+`)
	digitsRE = regexp.MustCompile(`\d[\d \-]{5,}\d`)
)

// externalCall 报告调用的能力是否来自外部来源：路由到设备、沙箱的能力与 MCP 工具的名称都含 "__"
// （"<label>__<能力>"、"sandbox__<能力>"、"mcp_<server>__<工具>"），进程内能力不含。
func externalCall(capability string) bool { return strings.Contains(capability, "__") }

// tainted 报告当前模型上下文中是否有不是用户原话的内容：
//   - 外部来源的调用结果（含后台调用迟到的结果）；压缩摘要可能概括了它们，保守地视为外部；
//   - 输入或回答附带的界面上下文，可能含第三方内容（docs/design/m3-duplex-channel.md §8）；
//   - Call 中语音模型的转述（派生的任务、转述的回答）：可能夹带用户没说过的内容（docs/design/m3-call.md §6）；
//   - 委托派生的子 Session：任务由父 Run 的模型写成，可能转述了父 Run 读到的外部内容（docs/design/m5-multi-agent.md §8）；
//   - 旁听转写中他人的话（ADR-0036）：佩戴者的话是用户本人的输入，不算。
func tainted(st *session.State) bool {
	if st.Compaction != nil || st.Created.GetParent() != nil {
		return true
	}
	external := map[string]bool{}
	for _, e := range st.History {
		if hasUIContext(e.GetRunRequested().GetInput()) || hasUIContext(e.GetSteered().GetInput()) || hasUIContext(e.GetToolResult().GetContent()) {
			return true
		}
		// 后台调用迟到的结果带着设备返回的内容（ADR-0035），与设备调用的结果一样是外部内容；到期没有内容。
		if deviceResult(e.GetRunRequested().GetInput()) || deviceResult(e.GetSteered().GetInput()) {
			return true
		}
		if e.GetRunRequested().GetFromCall() != "" || e.GetSteered().GetFromCall() != "" || e.GetToolResult().GetFromCall() != "" {
			return true
		}
		if len(otherSpeech(e.GetToolResult().GetContent())) > 0 {
			return true
		}
		if m := e.GetAssistantMessage(); m != nil {
			for _, tc := range m.GetToolCalls() {
				if externalCall(tc.GetCapability()) {
					external[tc.GetCallId()] = true
				}
			}
		}
		// 转入后台的占位结果由平台写成，不含设备的内容（ADR-0035）。
		if r := e.GetToolResult(); r != nil && external[r.GetCallId()] && r.GetBackgrounded() == nil {
			return true
		}
	}
	return false
}

// otherSpeech 返回内容中他人（不是佩戴者）没有被拦截的旁听片段的文字：它们是外部内容（ADR-0036）。
func otherSpeech(blocks []*v1.ContentBlock) []string {
	var out []string
	for _, c := range blocks {
		if g := c.GetCapture(); g.GetSpeaker() == v1.Speaker_SPEAKER_OTHER && !g.GetRedacted() {
			out = append(out, g.GetText())
		}
	}
	return out
}

// deviceResult 报告输入是否是后台调用迟到的设备结果。
func deviceResult(input []*v1.ContentBlock) bool {
	return session.BackgroundInput(input).GetReason() == session.BackgroundReasonResult
}

// hasUIContext 报告输入是否带有界面内容（可能来自第三方的外部内容）。只注明了用户所在设备的界面上下文由平台填写，
// 不算外部内容。
func hasUIContext(blocks []*v1.ContentBlock) bool {
	for _, b := range blocks {
		if u := b.GetUiContext(); u != nil && (u.GetScreen() != "" || u.GetRef() != "" || u.GetSelection() != "" || u.GetContent() != "") {
			return true
		}
	}
	return false
}

// userText 是上下文中 EndUser 本人的输入：新 Run、插话、对 ask_user 提问的回答（ADR-0025）、Call 中
// 用户说的话（转写），以及旁听转写中佩戴者的话（ADR-0036）。Call 派生的任务与转述的回答是语音模型的转述，不是用户的原话，不计入（docs/design/m3-call.md §6）；
// 委托给子 Session 的任务由父 Run 的模型写成，同样不计入。
func userText(st *session.State) string {
	var b strings.Builder
	questions := map[string]bool{}
	delegated := ""
	if p := st.Created.GetParent(); p != nil {
		delegated = delegation.InputID(p.GetCallId())
	}
	for _, e := range st.History {
		switch p := e.GetPayload().(type) {
		case *v1.Event_RunRequested:
			if p.RunRequested.GetFromCall() == "" && (delegated == "" || p.RunRequested.GetInputId() != delegated) {
				b.WriteString(model.Text(p.RunRequested.GetInput()))
			}
		case *v1.Event_Steered:
			if p.Steered.GetFromCall() == "" {
				b.WriteString(model.Text(p.Steered.GetInput()))
			}
		case *v1.Event_CallTranscript:
			if p.CallTranscript.GetRole() == "user" {
				b.WriteString(p.CallTranscript.GetText())
			}
		case *v1.Event_AssistantMessage:
			for _, tc := range p.AssistantMessage.GetToolCalls() {
				if tc.GetCapability() == askuser.Capability {
					questions[tc.GetCallId()] = true
				}
			}
		case *v1.Event_ToolResult:
			if questions[p.ToolResult.GetCallId()] && !p.ToolResult.GetIsError() && p.ToolResult.GetFromCall() == "" {
				b.WriteString(model.Text(p.ToolResult.GetContent()))
			}
			// 旁听片段只能由平台写入 captures 的结果（session 校验），说话人是结构化字段而不是文本中的前缀。
			for _, c := range p.ToolResult.GetContent() {
				if g := c.GetCapture(); g.GetSpeaker() == v1.Speaker_SPEAKER_WEARER && !g.GetRedacted() {
					b.WriteString(g.GetText())
					b.WriteByte('\n')
				}
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// grounded 报告 content 是否能在用户输入中找到依据：其中的邮箱、链接、号码都原样出现在用户输入里，
// 且 content 的字符二元组至少一半出现在用户输入中（允许"我对花生过敏"→"用户对花生过敏"这样的改写）。
// 投毒通常夹带用户从未说过的地址或号码，前一条最能识别它。
func grounded(content, user string) bool {
	compact := func(s string) string { return strings.NewReplacer(" ", "", "-", "").Replace(s) }
	for _, re := range []*regexp.Regexp{emailRE, urlRE} {
		for _, id := range re.FindAllString(content, -1) {
			if !strings.Contains(user, id) {
				return false
			}
		}
	}
	for _, id := range digitsRE.FindAllString(content, -1) {
		if !strings.Contains(compact(user), compact(id)) {
			return false
		}
	}
	grams := bigrams(content)
	if len(grams) == 0 {
		return true
	}
	have := bigrams(user)
	hit := 0
	for g := range grams {
		if have[g] {
			hit++
		}
	}
	return 2*hit >= len(grams)
}

// bigrams 返回字母、数字与汉字组成的字符二元组（忽略标点与空白）。
func bigrams(s string) map[string]bool {
	var rs []rune
	for _, r := range strings.ToLower(s) {
		if r >= 0x4e00 && r <= 0x9fff || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			rs = append(rs, r)
		}
	}
	out := map[string]bool{}
	for i := 0; i+1 < len(rs); i++ {
		out[string(rs[i:i+2])] = true
	}
	return out
}

// memoryGate 返回这次调用是否需要因写入闸门而审批，以及给用户看的说明。
func memoryGate(st *session.State, call *v1.ToolCall) (string, bool) {
	if call.GetCapability() != memoryGateCapability || !tainted(st) {
		return "", false
	}
	var in struct {
		Content string `json:"content"`
	}
	if json.Unmarshal([]byte(call.GetArgumentsJson()), &in) != nil || strings.TrimSpace(in.Content) == "" {
		return "", false // 参数无效时由 memory_save 自己拒绝
	}
	if grounded(in.Content, userText(st)) {
		return "", false
	}
	summary, _ := model.Truncate(in.Content, 200)
	return fmt.Sprintf("助手想记住：%s（这条信息来自本次对话中读取的外部内容，不是你直接说的）", summary), true
}
