package runtime

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	v1 "yanshi/gen/yanshi/v1"
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

// tainted 报告当前模型上下文中是否有外部来源的内容。压缩摘要可能概括了外部结果，保守地视为外部。
func tainted(st *session.State) bool {
	if st.Compaction != nil {
		return true
	}
	external := map[string]bool{}
	for _, e := range st.History {
		if m := e.GetAssistantMessage(); m != nil {
			for _, tc := range m.GetToolCalls() {
				if externalCall(tc.GetCapability()) {
					external[tc.GetCallId()] = true
				}
			}
		}
		if r := e.GetToolResult(); r != nil && external[r.GetCallId()] {
			return true
		}
	}
	return false
}

// userText 是上下文中 EndUser 本人的输入（新 Run 与插话）。
func userText(st *session.State) string {
	var b strings.Builder
	for _, e := range st.History {
		switch p := e.GetPayload().(type) {
		case *v1.Event_RunRequested:
			b.WriteString(model.Text(p.RunRequested.GetInput()))
		case *v1.Event_Steered:
			b.WriteString(model.Text(p.Steered.GetInput()))
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
