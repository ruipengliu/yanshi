package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/sdk/nodesdk"
)

// TestSubmitCapturesOverConnection：HostApp 经 Connection 提交旁听转写（docs/design/captures.md §3）。写入日志、不开启 Run；
// 以同一 capture_id 重新提交（进程重启后补交）得到 duplicate；未开启旁听的 AgentDef 为 invalid；他人的 Session 为 not_found。
func TestSubmitCapturesOverConnection(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	phone := e.startClient("phone", userToken, nil)
	sid, err := phone.CreateSession(ctx, "capture", "")
	if err != nil {
		t.Fatal(err)
	}
	in := &v1.SubmitCaptures{SessionId: sid, CaptureId: nodesdk.NewCaptureID(), Segments: []*v1.CaptureSegment{
		{Speaker: v1.Speaker_SPEAKER_WEARER, Text: "明天我把报价发给张三"},
		{Speaker: v1.Speaker_SPEAKER_OTHER, SpeakerKey: "other-1", Text: "好的，周五之前"},
	}}
	resp, err := phone.SubmitCaptures(ctx, in)
	if err != nil || resp.GetDuplicate() || resp.GetRedacted() != 0 {
		t.Fatalf("submit = %v, %v", resp, err)
	}
	if resp, err = phone.SubmitCaptures(ctx, in); err != nil || !resp.GetDuplicate() {
		t.Fatalf("resubmit = %v, %v", resp, err)
	}

	var events struct {
		Events []json.RawMessage `json:"events"`
	}
	e.do(http.MethodGet, "/v1/sessions/"+sid+"/events", nil, &events)
	captures, runs := 0, 0
	for _, raw := range events.Events {
		if strings.Contains(string(raw), `"capture_appended"`) {
			captures++
		}
		if strings.Contains(string(raw), `"run_requested"`) {
			runs++
		}
	}
	if captures != 1 || runs != 0 {
		t.Fatalf("captures = %d, runs = %d", captures, runs)
	}

	var re *nodesdk.RequestError
	plain := e.newSession()
	if _, err := phone.SubmitCaptures(ctx, &v1.SubmitCaptures{SessionId: plain, CaptureId: nodesdk.NewCaptureID(),
		Segments: in.GetSegments()}); !errors.As(err, &re) || re.Code != "invalid" {
		t.Fatalf("capture into an agent without captures: %v", err)
	}
	other := func(context.Context) (string, error) {
		return blKey.Sign("yanshi", "someone-else", time.Hour, time.Now())
	}
	theirs := e.startClient("theirs", other, nil)
	if _, err := theirs.SubmitCaptures(ctx, &v1.SubmitCaptures{SessionId: sid, CaptureId: nodesdk.NewCaptureID(),
		Segments: in.GetSegments()}); !errors.As(err, &re) || re.Code != "not_found" {
		t.Fatalf("capture into a foreign session: %v", err)
	}
	if _, err := phone.SubmitCaptures(ctx, &v1.SubmitCaptures{SessionId: sid, Segments: in.GetSegments()}); err == nil {
		t.Fatal("submitted captures without an id")
	}
}
