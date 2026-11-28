package session

import (
	"fmt"
	"time"
)

// ActiveCall 是进行中的 Call。注意与 Call（Run 中的一次 Capability 调用）区分。
type ActiveCall struct {
	ID        string
	DeviceID  string
	StartedAt time.Time
}

// CallRoles 是 CallTranscript.role 的合法取值。
var CallRoles = []string{"user", "assistant"}

// checkCall 要求 id 是进行中的 Call。
func (s *State) checkCall(id string) error {
	if s.ActiveCall == nil || s.ActiveCall.ID != id {
		return fmt.Errorf("call %q is not active", id)
	}
	return nil
}

// checkFromCall 要求由 Call 派生的输入来自进行中的 Call：结束的 Call 不再派生任务。
func (s *State) checkFromCall(id string) error {
	if id == "" {
		return nil
	}
	return s.checkCall(id)
}
