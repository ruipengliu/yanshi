package feed

import (
	"testing"

	v1 "yanshi/gen/yanshi/v1"
)

// 外部循环的状态只用于下一步请求，不发给客户端（ADR-0062 第 2 条）；原事件不被修改。
func TestPublicStripsLoopState(t *testing.T) {
	e := &v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{
		RunId: "r", LoopState: []byte("s"), LoopStateVersion: "v"}}}
	got := Public(e).GetAssistantMessage()
	if got.GetLoopState() != nil || got.GetLoopStateVersion() != "" || got.GetRunId() != "r" {
		t.Fatalf("public %v", got)
	}
	if string(e.GetAssistantMessage().GetLoopState()) != "s" {
		t.Fatal("original event modified")
	}
	plain := &v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{RunId: "r"}}}
	if Public(plain) != plain {
		t.Fatal("event without loop state copied")
	}
}
