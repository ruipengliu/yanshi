package service_test

import (
	"context"
	"errors"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue/memqueue"
)

func callService(t *testing.T) (*service.Service, string) {
	t.Helper()
	agents, _ := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
	store := &session.Store{Log: memlog.New(), IDs: ids.Sequential("id"), Clock: clock.Real{}}
	svc := &service.Service{Store: store, Queue: memqueue.New(clock.Real{}), Agents: agents, Moderator: moderation.Mock{}}
	sid, err := svc.Create(context.Background(), service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "a"})
	if err != nil {
		t.Fatal(err)
	}
	return svc, sid
}

func line(call, role, text string) *v1.CallTranscript {
	return &v1.CallTranscript{CallId: call, Role: role, Text: text}
}

// TestCallLifecycle：Call 的开始、转写、派生任务与结束都经 service 写入；新 Call 取代旧的；
// 结束的 Call 不能再写转写或派生任务；关闭 Session 时 Call 随之结束。
func TestCallLifecycle(t *testing.T) {
	ctx := context.Background()
	svc, sid := callService(t)
	var meterIDs []string
	start := func(id string) error {
		cs, err := svc.StartCall(ctx, service.StartCallRequest{SessionID: sid, CallID: id, DeviceID: "phone", Model: "volc/x"})
		if err == nil {
			if cs.State.ActiveCall == nil || cs.State.ActiveCall.ID != id {
				t.Fatalf("state after start has active call %+v, want %s", cs.State.ActiveCall, id)
			}
			meterIDs = append(meterIDs, cs.MeterID)
		}
		return err
	}
	if err := start("a"); err != nil {
		t.Fatal(err)
	}
	if err := start("a"); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("restarting the same call: %v", err)
	}
	if err := svc.AppendTranscript(ctx, sid, line("a", "user", "帮我看看 PPT")); err != nil {
		t.Fatal(err)
	}
	res, err := svc.SubmitFromCall(ctx, sid, "a", "a/t1", model.TextBlocks("读取 PPT 第三页"))
	if err != nil || res.Steered {
		t.Fatalf("run from call: %+v %v", res, err)
	}
	if again, err := svc.SubmitFromCall(ctx, sid, "a", "a/t1", model.TextBlocks("读取 PPT 第三页")); err != nil || !again.Duplicate || again.RunID != res.RunID {
		t.Fatalf("retried task: %+v %v", again, err)
	}
	// 新 Call 取代旧 Call：旧 Call 的写入一律冲突，结束旧 Call 是无操作。
	if err := start("b"); err != nil {
		t.Fatal(err)
	}
	if len(meterIDs) != 2 || meterIDs[0] == "" || meterIDs[0] == meterIDs[1] {
		t.Fatalf("meter ids %q: each call needs its own server-generated id", meterIDs)
	}
	if err := svc.AppendTranscript(ctx, sid, line("a", "assistant", "好的")); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("transcript of a replaced call: %v", err)
	}
	if _, err := svc.SubmitFromCall(ctx, sid, "a", "a/t2", model.TextBlocks("x")); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("task from a replaced call: %v", err)
	}
	if err := svc.EndCall(ctx, sid, "a", service.CallEndHangup); err != nil {
		t.Fatal(err)
	}
	// 违规的话不写入。
	if err := svc.AppendTranscript(ctx, sid, line("b", "assistant", "【违规测试】")); !errors.Is(err, moderation.ErrRejected) {
		t.Fatalf("moderated transcript: %v", err)
	}
	if err := svc.AppendTranscript(ctx, sid, line("b", "narrator", "x")); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("bad role: %v", err)
	}
	if err := svc.Close(ctx, sid, "user", ""); err != nil {
		t.Fatal(err)
	}
	st, _ := svc.Load(ctx, sid)
	if st.ActiveCall != nil || st.Closed == nil {
		t.Fatalf("after close: call %+v", st.ActiveCall)
	}
	events, _ := eventlog.ReadAll(ctx, svc.Store.Log, sid, 0)
	var got []string
	for _, e := range events {
		switch p := e.GetPayload().(type) {
		case *v1.Event_CallStarted:
			got = append(got, "start:"+p.CallStarted.GetCallId())
		case *v1.Event_CallTranscript:
			got = append(got, p.CallTranscript.GetRole()+":"+p.CallTranscript.GetText())
		case *v1.Event_CallEnded:
			got = append(got, "end:"+p.CallEnded.GetCallId()+":"+p.CallEnded.GetReason())
		case *v1.Event_RunRequested:
			got = append(got, "run:"+p.RunRequested.GetFromCall())
		}
	}
	want := []string{"start:a", "user:帮我看看 PPT", "run:a", "end:a:replaced", "start:b", "end:b:session_closed"}
	if len(got) != len(want) {
		t.Fatalf("log = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("log = %v, want %v", got, want)
		}
	}
	if err := start("c"); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("call in a closed session: %v", err)
	}
}
