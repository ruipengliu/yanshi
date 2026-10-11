package script

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"yanshi/internal/model"
)

func user(s string) model.Message {
	return model.Message{Role: model.RoleUser, Content: model.TextBlocks(s)}
}
func tool(s string) model.Message {
	return model.Message{Role: model.RoleTool, Content: model.TextBlocks(s)}
}
func assistant() model.Message { return model.Message{Role: model.RoleAssistant} }

var tools = []model.ToolSpec{{Name: "ask_user"}, {Name: "a-phone__get_location"}, {Name: "macbook__read_file"}, {Name: "phone__get_location"}}

func generate(t *testing.T, msgs ...model.Message) (*model.Response, string) {
	t.Helper()
	var deltas strings.Builder
	resp, err := Provider{}.Generate(context.Background(), &model.Request{Model: "any", Messages: msgs, Tools: tools},
		func(d model.Delta) { deltas.WriteString(d.Text) })
	if err != nil {
		t.Fatal(err)
	}
	return resp, deltas.String()
}

func TestCallsRunInOrderThenDone(t *testing.T) {
	in := user("call read_file {\"path\":\"a.txt\"}\ncall get_location")
	resp, _ := generate(t, in)
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].GetCapability() != "macbook__read_file" || resp.ToolCalls[0].GetArgumentsJson() != `{"path":"a.txt"}` {
		t.Fatalf("first step = %v", resp.ToolCalls)
	}
	resp, _ = generate(t, in, assistant(), tool("hello"))
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].GetCapability() != "a-phone__get_location" || resp.ToolCalls[0].GetArgumentsJson() != "{}" {
		t.Fatalf("second step = %v", resp.ToolCalls)
	}
	resp, deltas := generate(t, in, assistant(), tool("hello"), assistant(), tool("beijing"))
	if len(resp.ToolCalls) != 0 || model.Text(resp.Content) != "done: beijing" || deltas != "done: beijing" {
		t.Fatalf("final = %q (deltas %q)", model.Text(resp.Content), deltas)
	}
}

func TestSteerStartsANewScript(t *testing.T) {
	resp, _ := generate(t, user("call read_file"), assistant(), tool("x"), user("你好"))
	if model.Text(resp.Content) != "echo: 你好" {
		t.Fatalf("steer = %q", model.Text(resp.Content))
	}
}

func TestAskBuildsQuestion(t *testing.T) {
	resp, _ := generate(t, user("ask 选哪天？| 周四 | 周五"))
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].GetCapability() != "ask_user" {
		t.Fatalf("ask = %v", resp.ToolCalls)
	}
	var a struct {
		Question string
		Options  []struct{ ID, Label string }
	}
	if err := json.Unmarshal([]byte(resp.ToolCalls[0].GetArgumentsJson()), &a); err != nil {
		t.Fatal(err)
	}
	if a.Question != "选哪天？" || len(a.Options) != 2 || a.Options[1].ID != "2" || a.Options[1].Label != "周五" {
		t.Fatalf("args = %+v", a)
	}
}

func TestStreamAndUnknownTool(t *testing.T) {
	resp, deltas := generate(t, user("stream 3"))
	if model.Text(resp.Content) != "streamed" || deltas != "chunk0 chunk1 chunk2 " {
		t.Fatalf("stream = %q, deltas %q", model.Text(resp.Content), deltas)
	}
	resp, _ = generate(t, user("call send_sms"))
	if len(resp.ToolCalls) != 0 || model.Text(resp.Content) != "no such tool: send_sms" {
		t.Fatalf("unknown tool = %q", model.Text(resp.Content))
	}
}

func TestCallTranscriptsAreSkipped(t *testing.T) {
	resp, _ := generate(t, user("call phone__get_location"), user("[语音通话] 用户：好的"))
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].GetCapability() != "phone__get_location" {
		t.Fatalf("transcript treated as a script: %v", resp.ToolCalls)
	}
}
