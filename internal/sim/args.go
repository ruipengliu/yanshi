package sim

import (
	"encoding/json"
	"fmt"
	"strings"

	v1 "yanshi/gen/yanshi/v1"
)

// 不合法的调用参数的模拟：模型偶尔给出不是 JSON 对象的参数（结尾多一个 }、混进标记）。这样的调用不审批、不派发、
// 不执行，错误作为结果交还模型（docs/design/m0-core-primitives.md §4）。

// badArgs 是模型给出过的几种坏法，以及合法 JSON 但不是对象的参数。
var badArgs = []string{
	`{"pad":"p"}}`,
	`{"habits":[],"suggestions" string="false">[]}`,
	`["pad"]`,
	`null`,
}

// invalidArgs 按一定概率返回不合法的参数。
func (w *World) invalidArgs() (string, bool) {
	if !w.chance(0.02) {
		return "", false
	}
	w.Stats.InvalidArgs++
	return badArgs[w.rng.IntN(len(badArgs))], true
}

// validObject 独立于运行时判断参数是否合法：空串按 {} 处理，其余必须是 JSON 对象。
func validObject(args string) bool {
	s := strings.TrimSpace(args)
	if s == "" {
		return true
	}
	var v map[string]json.RawMessage
	return json.Unmarshal([]byte(s), &v) == nil && v != nil
}

// checkInvalidArgs 检查参数不合法的调用：没有请求审批、没有开始（不执行、不派发），有结果时是错误结果。
func checkInvalidArgs(sid string, events []*v1.Event) error {
	type key struct{ run, call string }
	bad := map[key]bool{}
	for _, e := range events {
		if m := e.GetAssistantMessage(); m != nil {
			for _, c := range m.GetToolCalls() {
				if !validObject(c.GetArgumentsJson()) {
					bad[key{m.GetRunId(), c.GetCallId()}] = true
				}
			}
		}
	}
	for _, e := range events {
		switch {
		case bad[key{e.GetApprovalRequested().GetRunId(), e.GetApprovalRequested().GetCallId()}]:
			return fmt.Errorf("invariant: %s requested approval for call %s with invalid arguments", sid, e.GetApprovalRequested().GetCallId())
		case bad[key{e.GetToolCallStarted().GetRunId(), e.GetToolCallStarted().GetCallId()}]:
			return fmt.Errorf("invariant: %s started call %s with invalid arguments", sid, e.GetToolCallStarted().GetCallId())
		case bad[key{e.GetToolResult().GetRunId(), e.GetToolResult().GetCallId()}] && !e.GetToolResult().GetIsError():
			return fmt.Errorf("invariant: %s call %s with invalid arguments has a successful result", sid, e.GetToolResult().GetCallId())
		}
	}
	return nil
}
