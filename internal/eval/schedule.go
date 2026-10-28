package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
)

// offsetClock 保留真实时间的流逝，使拨钟后租约与超时仍能正常推进。
// 每个会拨钟的 trial 独占一个实例，偏移不会改变其他 trial 的时间。
type offsetClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *offsetClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *offsetClock) AdvanceTo(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if delta := at.Sub(time.Now().Add(c.offset)); delta > 0 {
		c.offset += delta
	}
}

func advancesSchedule(c *Case) bool {
	return slices.ContainsFunc(c.Turns, func(t Turn) bool { return t.AdvanceToNextSchedule })
}

func isolatedTrial(ctx context.Context, cfg Config, c *Case, trial int) TrialResult {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cfg.Parallel = 1
	in, err := start(ctx, cfg, []*Case{c})
	if err != nil {
		return TrialResult{Err: "start isolated trial: " + err.Error()}
	}
	defer func() {
		cancel()
		// 总清理销毁沙箱之前，所有实例的控制器必须停止创建。
		<-in.controllerDone
		in.close()
	}()
	return in.trial(ctx, c, trial)
}

func caseTimeZone(c *Case, agent string) string {
	if c.TimeZone != "" {
		return c.TimeZone
	}
	if c.Setup.Xinli != nil || strings.HasPrefix(agent, "xinli-") {
		return "Asia/Shanghai"
	}
	return ""
}

func (in *instance) advanceToNextSchedule(ctx context.Context, sid string) (string, error) {
	st, err := in.svc.Load(ctx, sid)
	if err != nil {
		return "", err
	}
	items := st.OpenSchedules()
	if len(items) == 0 {
		return "", fmt.Errorf("advance_to_next_schedule: session has no active schedule")
	}
	if st.Active() != nil {
		return "", fmt.Errorf("advance_to_next_schedule: session still has an active run")
	}
	item, after := items[0], st.Seq
	in.clock.AdvanceTo(item.NextDue)
	if err := in.queue.Enqueue(ctx, sid, st.WorkClass()); err != nil {
		return "", err
	}
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		events, err := eventlog.ReadAll(ctx, in.svc.Store.Log, sid, after)
		if err != nil {
			return "", err
		}
		for _, e := range events {
			r := e.GetRunRequested()
			if r.GetSchedule().GetScheduleId() == item.Created.GetScheduleId() {
				return r.GetRunId(), nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-tick.C:
		}
	}
}

func checkSchedules(add func(string, bool, string), e Expect, o *observation) {
	if len(e.CallSequence) > 0 {
		next := 0
		for i, name := range o.calls {
			want := e.CallSequence[next]
			var args bytes.Buffer
			if !matches([]string{want.Capability}, name) || json.Compact(&args, []byte(o.callArgs[i])) != nil {
				continue
			}
			if !slices.ContainsFunc(want.Contains, func(sub string) bool { return !strings.Contains(args.String(), sub) }) {
				next++
				if next == len(e.CallSequence) {
					break
				}
			}
		}
		add("call_sequence", next == len(e.CallSequence), fmt.Sprintf("matched %d of %d ordered calls", next, len(e.CallSequence)))
	}
	if want := e.Schedules; want != nil {
		if r := want.Active; r != nil {
			n := len(o.schedules)
			add("schedules active", (r.Min == nil || n >= *r.Min) && (r.Max == nil || n <= *r.Max), fmt.Sprintf("active schedules %d", n))
		}
		if len(want.RRuleContains) > 0 || want.TimeZone != "" {
			matched := false
			for _, item := range o.schedules {
				m := item.Created
				if (want.TimeZone == "" || m.GetTimeZone() == want.TimeZone) && !slices.ContainsFunc(want.RRuleContains, func(sub string) bool { return !strings.Contains(m.GetRrule(), sub) }) {
					matched = true
				}
			}
			add("schedules rule", matched, fmt.Sprintf("want rule containing %v, time zone %q", want.RRuleContains, want.TimeZone))
		}
	}
}

// scheduledRunEvents 防止同刻到期的下一个 Run 混入本轮回复、调用与评分材料。
// 保留收集器与子 Session 遍历需要的事件，其他 Session 级事件不参与本轮断言。
func scheduledRunEvents(events []*v1.Event, runID string) []*v1.Event {
	return slices.DeleteFunc(events, func(e *v1.Event) bool {
		switch p := e.GetPayload().(type) {
		case *v1.Event_RunRequested:
			return p.RunRequested.GetRunId() != runID
		case *v1.Event_AssistantMessage:
			return p.AssistantMessage.GetRunId() != runID
		case *v1.Event_ToolResult:
			return p.ToolResult.GetRunId() != runID
		case *v1.Event_ApprovalRequested:
			return p.ApprovalRequested.GetRunId() != runID
		case *v1.Event_ApprovalDecided:
			return p.ApprovalDecided.GetRunId() != runID
		case *v1.Event_ContextCompacted:
			return p.ContextCompacted.GetRunId() != runID
		case *v1.Event_ToolCallStarted:
			return p.ToolCallStarted.GetRunId() != runID
		default:
			return true
		}
	})
}
