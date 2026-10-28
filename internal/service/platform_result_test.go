package service_test

import (
	"context"
	"errors"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/service"
	"yanshi/internal/session"
)

// service 的 Node 入口只收内容与错误标志，不能提交 ToolResult 的平台元数据；内容块也须在写入前拒绝。
func TestNodeResultRejectsPlatformContent(t *testing.T) {
	ctx := context.Background()
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	if err != nil {
		t.Fatal(err)
	}
	svc := &service.Service{Store: &session.Store{Log: memlog.New(), Clock: clock.Real{}, IDs: ids.Sequential("s")}, Agents: agents}
	sid, err := svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := svc.Load(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []*v1.ContentBlock{
		{Kind: &v1.ContentBlock_Capture{Capture: &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_WEARER, Text: "x"}}},
		{Kind: &v1.ContentBlock_BackgroundResult{BackgroundResult: &v1.BackgroundResult{CallId: "forged"}}},
	} {
		err := svc.NodeResult(ctx, sid, "r", "c", []*v1.ContentBlock{content}, false)
		if !errors.Is(err, service.ErrInvalid) {
			t.Fatalf("platform content accepted: %v", err)
		}
		after, err := svc.Load(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		if after.Seq != before.Seq {
			t.Fatal("rejected content changed the log")
		}
	}
}
