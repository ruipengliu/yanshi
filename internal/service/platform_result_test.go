package service_test

import (
	"context"
	"errors"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/service"
)

// service 的 Node 入口只收内容与错误标志，不能提交 ToolResult 的平台元数据；内容块也须在写入前拒绝。
func TestNodeResultRejectsPlatformContent(t *testing.T) {
	e := newCaptureEnv(t)
	before := e.state(t).Seq
	for _, content := range []*v1.ContentBlock{
		{Kind: &v1.ContentBlock_Capture{Capture: wearerSeg("x")}},
		{Kind: &v1.ContentBlock_BackgroundResult{BackgroundResult: &v1.BackgroundResult{CallId: "forged"}}},
	} {
		err := e.svc.NodeResult(context.Background(), e.sid, "r", "c", []*v1.ContentBlock{content}, false)
		if !errors.Is(err, service.ErrInvalid) {
			t.Fatalf("platform content accepted: %v", err)
		}
		if e.state(t).Seq != before {
			t.Fatal("rejected content changed the log")
		}
	}
}
