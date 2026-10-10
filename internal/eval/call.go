package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/call"
	"yanshi/internal/eventlog"
	"yanshi/internal/realtime"
	"yanshi/internal/service"
)

// Call 用例（docs/design/m3-call.md §9）：每轮的 input 经语音合成成为"用户说的话"，按实时节奏送入一次真实的 Call，
// 由实时语音模型应答；需要做事时经 run_task 派生 Run（被评测的 AgentDef 与文字用例相同）。一轮在通话安静下来、
// 本轮派生的 Run 都已结束之后结束。

// callQuiet 是判定一轮结束所需的安静时长：没有进行中的回复、没有新的语音或转写。
const callQuiet = 3 * time.Second

// callRecorder 收集一次 Call 发给设备的消息。
type callRecorder struct {
	mu        sync.Mutex
	users     []string // 用户的整句
	replies   []string // 助手的整句
	busy      bool     // 有进行中的回复
	activity  time.Time
	runs      map[string]bool // 派生任务所在的 Run
	ended     string
	failedErr error
}

func (r *callRecorder) out(e *v1.CallEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.activity = time.Now()
	switch k := e.GetKind().(type) {
	case *v1.CallEvent_Text:
		if k.Text.GetRole() == "assistant" {
			r.busy = true
		}
		if k.Text.GetFinal() {
			if k.Text.GetRole() == "user" {
				r.users = append(r.users, k.Text.GetText())
			} else {
				r.replies = append(r.replies, k.Text.GetText())
			}
		}
	case *v1.CallEvent_Audio:
		r.busy = true
	case *v1.CallEvent_ResponseDone:
		r.busy = false
	case *v1.CallEvent_Task:
		r.runs[k.Task.GetRunId()] = true
	case *v1.CallEvent_Ended:
		r.ended = k.Ended.GetReason()
	}
	return nil
}

func (in *instance) startCall(ctx context.Context, sid string) (*call.Call, *callRecorder, error) {
	rec := &callRecorder{runs: map[string]bool{}, activity: time.Now()}
	c, err := in.calls.Start(ctx, call.StartRequest{SessionID: sid, CallID: "eval-call-" + sid, DeviceID: "eval-phone"}, rec.out)
	return c, rec, err
}

// callTurn 说出一轮的 input，等通话安静、本轮的 Run 结束，收集本轮的事件。
func (in *instance) callTurn(ctx context.Context, c *call.Call, rec *callRecorder, sid string, turn Turn, timeout time.Duration) (*observation, error) {
	if in.cfg.Speak == nil {
		return nil, errors.New("no speech synthesizer")
	}
	pcm, err := in.cfg.Speak(ctx, turn.Input)
	if err != nil {
		return nil, fmt.Errorf("synthesize: %w", err)
	}
	st, err := in.svc.Load(ctx, sid)
	if err != nil {
		return nil, err
	}
	after := st.Seq
	rec.mu.Lock()
	users, replies := len(rec.users), len(rec.replies)
	rec.mu.Unlock()
	// 按实时节奏送入：一次全部送入会超过网关的积压上限而被丢弃。
	tick := time.NewTicker(20 * time.Millisecond)
	for i := 0; i < len(pcm); i += realtime.FrameBytes {
		select {
		case <-ctx.Done():
			tick.Stop()
			return nil, ctx.Err()
		case <-tick.C:
		}
		frame := make([]byte, realtime.FrameBytes)
		copy(frame, pcm[i:min(i+realtime.FrameBytes, len(pcm))])
		c.Audio(frame)
	}
	tick.Stop()

	if timeout <= 0 {
		timeout = in.cfg.TurnTimeout
	}
	approve := turn.Approve == nil || *turn.Approve
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		st, err := in.svc.Load(ctx, sid)
		if err != nil {
			return nil, err
		}
		if a := st.Active(); a != nil {
			for _, cl := range a.Calls {
				if cl.AwaitingApproval() {
					if err := in.svc.Decide(ctx, sid, cl.Call.GetCallId(), approve, "eval"); err != nil && !errors.Is(err, service.ErrConflict) {
						return nil, err
					}
				}
			}
		}
		rec.mu.Lock()
		heard := len(rec.users) > users && len(rec.replies) > replies
		quiet := !rec.busy && time.Since(rec.activity) > callQuiet
		ended := rec.ended
		running := false
		for id := range rec.runs {
			if r := st.Run(id); r != nil && !r.Status.Terminal() {
				running = true
			}
		}
		rec.mu.Unlock()
		if ended != "" {
			return nil, fmt.Errorf("call ended: %s", ended)
		}
		if heard && quiet && !running {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("call turn did not settle within %s (heard %v, quiet %v, running %v)", timeout, heard, quiet, running)
		}
	}
	events, err := eventlog.ReadAll(ctx, in.svc.Store.Log, sid, after)
	if err != nil {
		return nil, err
	}
	rec.mu.Lock()
	obs := &observation{input: strings.Join(rec.users[users:], " "), reply: strings.Join(rec.replies[replies:], "\n"), status: "completed"}
	rec.mu.Unlock()
	// 回复只看语音（转写），Run 的文字回复由 collect 收进 reply 之前先放一边。
	spoken := obs.reply
	collect(events, obs)
	obs.reply = spoken
	st, err = in.svc.Load(ctx, sid)
	if err != nil {
		return nil, err
	}
	obs.compacted = st.Compaction != nil
	for _, e := range events {
		var text, run string
		switch p := e.GetPayload().(type) {
		case *v1.Event_RunRequested:
			if p.RunRequested.GetFromCall() == "" {
				continue
			}
			text, run = blocksText(p.RunRequested.GetInput()), p.RunRequested.GetRunId()
		case *v1.Event_Steered:
			if p.Steered.GetFromCall() == "" {
				continue
			}
			text, run = blocksText(p.Steered.GetInput()), p.Steered.GetRunId()
		default:
			continue
		}
		obs.tasks = append(obs.tasks, text)
		obs.steps = append(obs.steps, "派生任务："+clip(text, 200))
		if r := st.Run(run); r != nil && r.Status.String() != "completed" {
			obs.status = r.Status.String()
		}
	}
	return obs, nil
}

func blocksText(blocks []*v1.ContentBlock) string {
	var b strings.Builder
	for _, x := range blocks {
		b.WriteString(x.GetText().GetText())
	}
	return b.String()
}
