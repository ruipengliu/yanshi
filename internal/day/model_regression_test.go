package day

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capture"
	"yanshi/internal/model"
	"yanshi/internal/runtime"
	"yanshi/internal/session"
)

// 必须经过真实 Transcript；直接把 Capture 块交给 Provider 会掩盖提供商只见文字的边界。
func TestDayModelProcessesTranscriptCapturePages(t *testing.T) {
	events := []*v1.Event{
		{Payload: &v1.Event_SessionCreated{SessionCreated: &v1.SessionCreated{}}},
		{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "run", Input: model.TextBlocks("整理今天")}}},
		{Payload: &v1.Event_AttemptStarted{AttemptStarted: &v1.AttemptStarted{RunId: "run", Attempt: 1}}},
	}
	tools := []model.ToolSpec{}
	for _, name := range []string{"captures", "find_related", "file_changes", "day_ledger", "file_day"} {
		tools = append(tools, model.ToolSpec{Name: "mcp_xinli__" + name})
	}
	request := func() *model.Request {
		for i, event := range events {
			event.SessionId, event.Seq = "session", uint64(i+1)
		}
		state, err := session.Reduce(events)
		must(t, err)
		return &model.Request{Model: "xinli-day", Tools: tools, Messages: runtime.Transcript(state, 0)}
	}
	step := func(name string, result []*v1.ContentBlock) *v1.ToolCall {
		t.Helper()
		req := request()
		for _, message := range req.Messages {
			for _, block := range message.Content {
				if block.GetCapture() != nil {
					t.Fatal("regression fixture bypassed capture expansion")
				}
			}
		}
		response, err := (dayModel{}).Generate(context.Background(), req, nil)
		must(t, err)
		if len(response.ToolCalls) != 1 || response.ToolCalls[0].GetCapability() != "mcp_xinli__"+name {
			t.Fatalf("script did not call expected tool %s", name)
		}
		call := response.ToolCalls[0]
		call.CallId = fmt.Sprintf("call_%d", len(events))
		events = append(events,
			&v1.Event{Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{RunId: "run", Attempt: 1, ToolCalls: []*v1.ToolCall{call}}}},
			&v1.Event{Payload: &v1.Event_ToolCallStarted{ToolCallStarted: &v1.ToolCallStarted{RunId: "run", Attempt: 1, CallId: call.CallId, ReadOnly: name == "captures" || name == "find_related" || name == "day_ledger"}}},
			&v1.Event{Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{RunId: "run", Attempt: 1, CallId: call.CallId, Content: result, Attested: name == "captures"}}})
		return call
	}
	page := func(seq uint64, index int, text string, cont, done, ended bool) []*v1.ContentBlock {
		g := &v1.CaptureSegment{Ref: capture.Ref{Seq: seq, Index: index}.String(), Speaker: v1.Speaker_SPEAKER_WEARER, Text: text,
			SpokenAt: timestamppb.New(time.Date(2035, 1, 8, int(seq), 30+index, 0, 0, time.UTC)), TimeZone: "Asia/Shanghai"}
		blocks, _, err := capture.Render(capture.RenderPage{Kind: "page", Span: capture.Ref{Seq: seq}, Cont: cont, Done: done, Ended: ended, Segments: []*v1.CaptureSegment{g}}, 0)
		must(t, err)
		return blocks
	}
	step("captures", page(1, 0, "张总，办公室装修的报价单我明天发给你。", false, false, false))
	step("captures", page(1, 1, "闲聊：好。", true, false, true))
	step("find_related", model.TextBlocks("没有相关承诺"))
	first := step("file_changes", model.TextBlocks("已收下"))
	var change struct {
		Source      struct{ From, To string }
		Commitments struct{ Create []struct{ Content string } }
	}
	must(t, json.Unmarshal([]byte(first.GetArgumentsJson()), &change))
	if change.Source.From != "#1.0" || change.Source.To != "#1.1" || len(change.Commitments.Create) != 1 || change.Commitments.Create[0].Content != "发报价单" {
		t.Fatal("continued transcript pages did not produce one complete source")
	}
	step("captures", page(2, 0, "李总，部门预算表我明天发给你。", false, true, true))
	step("find_related", model.TextBlocks("没有相关承诺"))
	second := step("file_changes", model.TextBlocks("已收下"))
	must(t, json.Unmarshal([]byte(second.GetArgumentsJson()), &change))
	if change.Source.From != "#2.0" || change.Source.To != "#2.0" || len(change.Commitments.Create) != 1 || change.Commitments.Create[0].Content != "发预算表" {
		t.Fatal("second transcript page lost its source or reused the previous span")
	}
	step("day_ledger", model.TextBlocks("当天账本"))
	step("file_day", model.TextBlocks("已收下当天总览"))
	response, err := (dayModel{}).Generate(context.Background(), request(), nil)
	must(t, err)
	if len(response.ToolCalls) != 0 || response.FinishReason != "stop" {
		t.Fatal("script did not finish after both sourced changes and file_day")
	}
}

func TestDayModelRejectsUnusableCaptureResult(t *testing.T) {
	for _, text := range []string{"[error] capture unavailable", "这是一页无法识别的非空旁听"} {
		req := &model.Request{Model: "xinli-day", Tools: []model.ToolSpec{{Name: "mcp_xinli__day_ledger"}}, Messages: []model.Message{
			{Role: model.RoleUser, Content: model.TextBlocks("整理今天")},
			{Role: model.RoleAssistant, ToolCalls: []*v1.ToolCall{{CallId: "capture", Capability: "mcp_xinli__captures"}}},
			{Role: model.RoleTool, ToolCallID: "capture", Content: model.TextBlocks(text)},
		}}
		if _, err := (dayModel{}).Generate(context.Background(), req, nil); err == nil {
			t.Fatal("script silently filed an unusable capture page")
		}
	}
}

func TestDayModelLeavesOpenSpanForLater(t *testing.T) {
	blocks, _, err := capture.Render(capture.RenderPage{Kind: "in_progress", Span: capture.Ref{Seq: 1}, LastAt: time.Date(2035, 1, 8, 1, 0, 0, 0, time.UTC)}, 0)
	must(t, err)
	req := &model.Request{Model: "xinli-day", Messages: []model.Message{
		{Role: model.RoleUser, Content: model.TextBlocks("整理交谈")},
		{Role: model.RoleAssistant, ToolCalls: []*v1.ToolCall{{CallId: "capture", Capability: "mcp_xinli__captures"}}},
		{Role: model.RoleTool, ToolCallID: "capture", Content: blocks},
	}}
	response, err := (dayModel{}).Generate(context.Background(), req, nil)
	must(t, err)
	if len(response.ToolCalls) != 0 || response.FinishReason != "stop" {
		t.Fatal("script filed a still-open span")
	}
}
