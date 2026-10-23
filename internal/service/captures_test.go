package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/moderation"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue/memqueue"
)

type captureEnv struct {
	svc   *service.Service
	clk   *clock.Fake
	index *lifecycle.MemIndex
	sid   string
	plain string // 未开启旁听的 AgentDef 的 Session
}

func newCaptureEnv(t *testing.T) *captureEnv {
	t.Helper()
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	agents, _ := agentdef.NewRegistry(
		&agentdef.Def{Name: "day", Version: "1", Model: "echo/any", Captures: true},
		&agentdef.Def{Name: "project", Version: "1", Model: "echo/any"},
	)
	mlog, mq := memlog.New(), memqueue.New(clk)
	store := &session.Store{Log: mlog, Waking: mlog.Waking(mq), IDs: ids.Sequential("id"), Clock: clk}
	e := &captureEnv{clk: clk, index: lifecycle.NewMemIndex()}
	e.svc = &service.Service{Store: store, Agents: agents, Index: e.index, Moderator: moderation.Mock{}}
	var err error
	if e.sid, err = e.svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "day"}); err != nil {
		t.Fatal(err)
	}
	if e.plain, err = e.svc.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "project"}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *captureEnv) state(t *testing.T) *session.State {
	t.Helper()
	st, err := e.svc.Load(context.Background(), e.sid)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func seg(speaker v1.Speaker, key, text string) *v1.CaptureSegment {
	return &v1.CaptureSegment{Speaker: speaker, SpeakerKey: key, Text: text}
}

func wearerSeg(text string) *v1.CaptureSegment { return seg(v1.Speaker_SPEAKER_WEARER, "", text) }
func otherSeg(text string) *v1.CaptureSegment  { return seg(v1.Speaker_SPEAKER_OTHER, "other-1", text) }

// 旁听转写写入日志但不开启 Run；同一 capture_id 重复提交不再写入；关闭后拒绝。
func TestSubmitCaptures(t *testing.T) {
	ctx := context.Background()
	e := newCaptureEnv(t)
	res, err := e.svc.SubmitCaptures(ctx, e.sid, "cap1", []*v1.CaptureSegment{wearerSeg("明天我把报价发给张三"), otherSeg("好的")})
	if err != nil || res.Duplicate || res.Redacted != 0 {
		t.Fatalf("submit = %+v, %v", res, err)
	}
	st := e.state(t)
	if st.Captures.Count != 1 || len(st.Runs) != 0 {
		t.Fatalf("captures = %d, runs = %d", st.Captures.Count, len(st.Runs))
	}
	seq := st.Seq
	res, err = e.svc.SubmitCaptures(ctx, e.sid, "cap1", []*v1.CaptureSegment{wearerSeg("重试")})
	if err != nil || !res.Duplicate || e.state(t).Seq != seq {
		t.Fatalf("duplicate = %+v, %v", res, err)
	}
	if err := e.svc.Close(ctx, e.sid, "test", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SubmitCaptures(ctx, e.sid, "cap2", []*v1.CaptureSegment{wearerSeg("x")}); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("after close: %v", err)
	}
}

func TestSubmitCapturesInvalid(t *testing.T) {
	ctx := context.Background()
	e := newCaptureEnv(t)
	redacted := &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_WEARER, Redacted: true}
	cases := map[string]struct {
		sid, id string
		segs    []*v1.CaptureSegment
		want    error
	}{
		"no id":             {e.sid, "", []*v1.CaptureSegment{wearerSeg("x")}, service.ErrInvalid},
		"long id":           {e.sid, strings.Repeat("x", service.MaxInputIDLen+1), []*v1.CaptureSegment{wearerSeg("x")}, service.ErrInvalid},
		"no segments":       {e.sid, "c", nil, service.ErrInvalid},
		"client redacted":   {e.sid, "c", []*v1.CaptureSegment{redacted}, service.ErrInvalid},
		"no speaker":        {e.sid, "c", []*v1.CaptureSegment{{Text: "x"}}, service.ErrInvalid},
		"agent not capture": {e.plain, "c", []*v1.CaptureSegment{wearerSeg("x")}, service.ErrInvalid},
		"unknown session":   {"ses_none", "c", []*v1.CaptureSegment{wearerSeg("x")}, service.ErrNotFound},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := e.svc.SubmitCaptures(ctx, c.sid, c.id, c.segs); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
	if e.state(t).Captures.Count != 0 {
		t.Fatal("invalid capture written")
	}
}

// pairModerator 只拦截同时含有两个词的文本：违规取决于上下文，单独看每一段都没有问题。
type pairModerator struct{ a, b string }

func (m pairModerator) Check(_ context.Context, req moderation.Request) (moderation.Verdict, error) {
	return moderation.Verdict{Block: strings.Contains(req.Text, m.a) && strings.Contains(req.Text, m.b)}, nil
}

// 违规的片段只留占位，其余照常写入；整批违规而没有一段单独违规时整批只留占位；内容安全不可用时整批拒绝（ADR-0021）。
func TestCaptureModeration(t *testing.T) {
	ctx := context.Background()
	e := newCaptureEnv(t)
	res, err := e.svc.SubmitCaptures(ctx, e.sid, "cap1", []*v1.CaptureSegment{wearerSeg("你好"), otherSeg("【违规测试】"), otherSeg("再见")})
	if err != nil || res.Redacted != 1 {
		t.Fatalf("submit = %+v, %v", res, err)
	}
	segs := lastCapture(t, e).GetSegments()
	if segs[0].GetText() != "你好" || segs[1].GetText() != "" || !segs[1].GetRedacted() || segs[2].GetText() != "再见" {
		t.Fatalf("segments = %v", segs)
	}

	e.svc.Moderator = pairModerator{"甲", "乙"}
	if res, err = e.svc.SubmitCaptures(ctx, e.sid, "cap2", []*v1.CaptureSegment{wearerSeg("甲"), otherSeg("乙")}); err != nil || res.Redacted != 2 {
		t.Fatalf("contextual = %+v, %v", res, err)
	}
	for _, g := range lastCapture(t, e).GetSegments() {
		if !g.GetRedacted() || g.GetText() != "" {
			t.Fatalf("contextual segment kept: %v", g)
		}
	}

	seq := e.state(t).Seq
	e.svc.Moderator = failingModerator{}
	if _, err := e.svc.SubmitCaptures(ctx, e.sid, "cap3", []*v1.CaptureSegment{wearerSeg("x")}); !errors.Is(err, moderation.ErrUnavailable) || e.state(t).Seq != seq {
		t.Fatalf("passed while moderation was unavailable: %v", err)
	}
	// 重复提交不再检查：首次已经检查过，内容安全不可用也照常返回。
	if res, err := e.svc.SubmitCaptures(ctx, e.sid, "cap1", []*v1.CaptureSegment{wearerSeg("x")}); err != nil || !res.Duplicate {
		t.Fatalf("duplicate = %+v, %v", res, err)
	}
}

func lastCapture(t *testing.T, e *captureEnv) *v1.CaptureAppended {
	t.Helper()
	events, err := e.svc.Store.Log.Read(context.Background(), e.sid, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if c := events[i].GetCaptureAppended(); c != nil {
			return c
		}
	}
	t.Fatal("no capture")
	return nil
}

// 只有旁听、没有输入的 Session 同样更新索引的最后输入时间（按 TouchEvery 分段），不会被按空闲时长关闭。
func TestCapturesTouchIndex(t *testing.T) {
	ctx := context.Background()
	e := newCaptureEnv(t)
	lastInput := func() time.Time {
		t.Helper()
		x, err := e.index.Get(ctx, e.sid)
		if err != nil {
			t.Fatal(err)
		}
		return x.LastInputAt
	}
	created := lastInput()
	e.clk.Advance(time.Hour)
	if _, err := e.svc.SubmitCaptures(ctx, e.sid, "cap1", []*v1.CaptureSegment{wearerSeg("x")}); err != nil {
		t.Fatal(err)
	}
	first := lastInput()
	if !first.After(created) {
		t.Fatalf("index not touched: %v", first)
	}
	// 同一时段内的下一批不再写索引。
	e.clk.Advance(lifecycle.TouchEvery / 10)
	if _, err := e.svc.SubmitCaptures(ctx, e.sid, "cap2", []*v1.CaptureSegment{wearerSeg("x")}); err != nil {
		t.Fatal(err)
	}
	if !lastInput().Equal(first) {
		t.Fatal("touched within the same TouchEvery segment")
	}
	e.clk.Advance(lifecycle.TouchEvery)
	if _, err := e.svc.SubmitCaptures(ctx, e.sid, "cap3", []*v1.CaptureSegment{wearerSeg("x")}); err != nil {
		t.Fatal(err)
	}
	if !lastInput().After(first) {
		t.Fatal("index not touched in a new segment")
	}
}
