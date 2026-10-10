package call

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/askuser"
	"yanshi/internal/feed"
	"yanshi/internal/live"
	"yanshi/internal/realtime"
	"yanshi/internal/service"
	"yanshi/internal/session"
)

// RunTask 是语音模型唯一的工具：把需要能力的工作交给 Session 的 Agent。
const RunTask = "run_task"

// RunTaskTool 的说明与各种结果文字都是给模型看的：改动时运行 make eval（evals/call）。
var RunTaskTool = realtime.Tool{
	Name: RunTask,
	Description: "把需要实际去做的事交给后台助手：读取或修改用户电脑、手机上的文件和应用，查当前时间或资料、计算、写文档、" +
		"发消息；用户让你记住、记下的事（偏好、健康状况、人际关系、日程）也必须交给它保存，你自己在通话结束后记不住。" +
		"后台助手能使用用户的设备和工具，也能看到通话记录，但 task 里仍要写清楚要做什么、对象是哪个、用户提到的关键细节。" +
		"闲聊、常识问答直接回答，不要调用。" +
		"返回结果后用一两句口语告诉用户；返回\"正在后台进行\"时告诉用户你在处理、完成后会告诉他，不要等待。" +
		"用户回答了后台助手的提问，或要补充、修改、取消正在进行的任务时，也用它转达用户的原话。",
	ParametersJSON: `{"type":"object","properties":{"task":{"type":"string","description":"交给后台助手的任务，写清楚要做什么"}},"required":["task"]}`,
}

// 工具结果与播报的文字。
const (
	runningText = "任务已经开始，正在后台进行，完成后会自动告诉用户。请简短告诉用户你正在处理，不用等。"
	mergedText  = "这个任务已并入用户后来的指示，由后续结果一并告知。"
	maxToolText = 1000
	maxSpoken   = 300
)

func clipRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > n {
		return string([]rune(s)[:n]) + "……"
	}
	return s
}

// runState 是本 Call 开始之后、跟随日志得到的一个 Run 的进展。
type runState struct {
	lastText  string
	status    session.RunStatus
	failure   string
	approvals map[string]string // call_id → 摘要：等待审批的调用
	questions map[string]string // call_id → 问题：等待回答的提问
	asks      map[string]string // call_id → ask_user 参数：模型发出的提问（尚未开始等待）
}

func newRunState() *runState {
	return &runState{status: session.RunQueued, approvals: map[string]string{}, questions: map[string]string{}, asks: map[string]string{}}
}

// outcome 是 Run 值得告诉用户的状态。
type outcome struct {
	// key 标识这一状态，同一状态只告知一次。
	key      string
	status   string
	tool     string
	spoken   string
	terminal bool
}

func (r *runState) outcome() *outcome {
	if r == nil {
		return nil
	}
	switch r.status {
	case session.RunCompleted:
		o := &outcome{key: "completed", status: "completed", terminal: true, tool: "任务完成。", spoken: "后台任务完成了。"}
		if r.lastText != "" {
			o.tool = "任务完成。后台助手的回复：" + clipRunes(r.lastText, maxToolText)
			o.spoken = clipRunes(r.lastText, maxSpoken)
		}
		return o
	case session.RunFailed:
		return &outcome{key: "failed", status: "failed", terminal: true,
			tool: "任务没有完成（" + r.failure + "）。请告诉用户。", spoken: "后台任务没有完成。"}
	case session.RunInterrupted:
		return &outcome{key: "interrupted", status: "interrupted", terminal: true, tool: "任务已经停止。", spoken: "后台任务已经停止了。"}
	}
	for id, summary := range r.approvals {
		return &outcome{key: "approval:" + id, status: "waiting",
			tool:   "任务需要用户批准才能继续：" + summary + "。请告诉用户在屏幕上确认或拒绝。",
			spoken: "后台任务需要你在屏幕上确认：" + summary}
	}
	for id, q := range r.questions {
		return &outcome{key: "question:" + id, status: "waiting",
			tool:   "后台助手需要用户回答：" + q + "。请问用户，用户回答后用 run_task 转达用户的回答。",
			spoken: "后台助手想问你：" + q}
	}
	return nil
}

// questionText 把 ask_user 的参数呈现为一句可以说出口的问题。
func questionText(args string) string {
	q, err := askuser.Parse(args)
	if err != nil {
		return "（问题无法解析）"
	}
	text := q.Question
	var labels []string
	for _, o := range q.Options {
		labels = append(labels, o.Label)
	}
	if len(labels) > 0 {
		text += "（选项：" + strings.Join(labels, "、") + "）"
	}
	return text
}

// watch 是派生任务所在的一个 Run：holder 是挂起等待结果的工具调用，until 是挂起的截止时间；
// reported 是已告知的状态。由 Call.mu 保护。
type watch struct {
	holder   string
	until    time.Time
	reported string
}

// seed 由开始时的投影初始化进行中的 Run（任务可能并入它）：之前就在等待的审批与提问也要能告知。
func (c *Call) seed(st *session.State) {
	a := st.Active()
	if a == nil {
		return
	}
	r := newRunState()
	r.status = a.Status
	for _, cl := range a.Calls {
		switch {
		case cl.AwaitingApproval():
			r.approvals[cl.Call.GetCallId()] = cl.Approval.Summary
		case cl.Dispatched() && cl.NodeID == askuser.NodeID:
			r.questions[cl.Call.GetCallId()] = questionText(cl.Call.GetArgumentsJson())
		}
	}
	c.mu.Lock()
	c.runs[a.ID] = r
	c.mu.Unlock()
}

// follow 跟随 Session 的日志，直到 Call 结束。
func (c *Call) follow(st *session.State) {
	defer c.wg.Done()
	c.seed(st)
	err := c.m.Feed.Stream(c.ctx, st, st.Seq, sink{c})
	switch {
	case c.ctx.Err() != nil:
	case errors.Is(err, feed.ErrDeleted):
		c.end(service.CallEndSessionClosed, false)
	case errors.Is(err, errCallEnded):
	case err != nil:
		c.m.log().Warn("call feed failed", "session", c.sid, "call", c.id, "err", err)
		c.End(service.CallEndError)
	}
}

var errCallEnded = errors.New("call ended")

type sink struct{ c *Call }

func (s sink) Event(e *v1.Event) error { return s.c.observe(e) }
func (sink) Delta(live.Delta) error    { return nil }
func (sink) Ping() error               { return nil }
func (sink) Flush() error              { return nil }

// observe 根据日志更新 Run 的进展；Call 在日志中结束（被取代、Session 关闭）时挂断。
func (c *Call) observe(e *v1.Event) error {
	if ce := e.GetCallEnded(); ce != nil && ce.GetCallId() == c.id {
		c.end(ce.GetReason(), false)
		return errCallEnded
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	run := func(id string) *runState {
		r := c.runs[id]
		if r == nil {
			r = newRunState()
			c.runs[id] = r
		}
		return r
	}
	switch p := e.GetPayload().(type) {
	case *v1.Event_AttemptStarted:
		run(p.AttemptStarted.GetRunId()).status = session.RunRunning
	case *v1.Event_AssistantMessage:
		r := run(p.AssistantMessage.GetRunId())
		if t := textOf(p.AssistantMessage.GetContent()); strings.TrimSpace(t) != "" {
			r.lastText = t
		}
		for _, tc := range p.AssistantMessage.GetToolCalls() {
			if tc.GetCapability() == askuser.Capability {
				r.asks[tc.GetCallId()] = tc.GetArgumentsJson()
			}
		}
	case *v1.Event_ApprovalRequested:
		run(p.ApprovalRequested.GetRunId()).approvals[p.ApprovalRequested.GetCallId()] = p.ApprovalRequested.GetSummary()
	case *v1.Event_ApprovalDecided:
		delete(run(p.ApprovalDecided.GetRunId()).approvals, p.ApprovalDecided.GetCallId())
	case *v1.Event_ToolCallStarted:
		m := p.ToolCallStarted
		r := run(m.GetRunId())
		delete(r.approvals, m.GetCallId())
		if m.GetNodeId() == askuser.NodeID {
			r.questions[m.GetCallId()] = questionText(r.asks[m.GetCallId()])
		}
	case *v1.Event_ToolResult:
		r := run(p.ToolResult.GetRunId())
		delete(r.questions, p.ToolResult.GetCallId())
		delete(r.approvals, p.ToolResult.GetCallId())
	case *v1.Event_RunCompleted:
		run(p.RunCompleted.GetRunId()).status = session.RunCompleted
	case *v1.Event_RunFailed:
		r := run(p.RunFailed.GetRunId())
		r.status, r.failure = session.RunFailed, p.RunFailed.GetReason()
	case *v1.Event_RunInterrupted:
		run(p.RunInterrupted.GetRunId()).status = session.RunInterrupted
	default:
		return nil
	}
	close(c.changed)
	c.changed = make(chan struct{})
	return nil
}

// watchRun 跟随任务所在的 Run：挂起期间（HoldFor 以内）把第一个值得告知的状态作为工具结果交还模型；
// 超时先回答"在后台进行"；之后的状态（完成、失败、需要审批或回答）在通话空闲时播报。同一 Run 只有一个跟随者：
// 后来的任务并入同一 Run 时，由它接管挂起。
func (c *Call) watchRun(runID, toolCallID string) {
	c.mu.Lock()
	if w := c.watches[runID]; w != nil {
		prev := w.holder
		w.holder, w.until = toolCallID, time.Now().Add(c.m.holdFor())
		close(c.changed)
		c.changed = make(chan struct{})
		c.mu.Unlock()
		if prev != "" {
			c.toolResult(prev, mergedText)
		}
		return
	}
	w := &watch{holder: toolCallID, until: time.Now().Add(c.m.holdFor())}
	c.watches[runID] = w
	c.mu.Unlock()
	for {
		c.mu.Lock()
		o := c.runs[runID].outcome()
		changed := c.changed
		var holder, reply, speak, status string
		done := false
		if o != nil && o.key != w.reported {
			w.reported, status, done = o.key, o.status, o.terminal
			if w.holder != "" {
				holder, reply, w.holder = w.holder, o.tool, ""
			} else {
				speak = o.spoken
			}
		} else if w.holder != "" && !time.Now().Before(w.until) {
			holder, reply, w.holder = w.holder, runningText, ""
		}
		if done {
			delete(c.watches, runID)
		}
		wait := time.Hour
		if w.holder != "" {
			wait = time.Until(w.until)
		}
		c.mu.Unlock()
		if holder != "" {
			c.toolResult(holder, reply)
		}
		if speak != "" {
			c.enqueueSpeech(speak)
		}
		if status != "" {
			c.send(&v1.CallEvent{Kind: &v1.CallEvent_Task{Task: &v1.CallTask{RunId: runID, Status: status}}})
		}
		if done {
			return
		}
		t := time.NewTimer(wait)
		select {
		case <-changed:
		case <-t.C:
		case <-c.ctx.Done():
			t.Stop()
			return
		}
		t.Stop()
	}
}

// answered 记下任务回答了 Run 的提问：日志中的结果可能稍后才跟随到，期间不应再把这个提问当作待回答。
func (c *Call) answered(runID, callID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.runs[runID]; r != nil {
		delete(r.questions, callID)
	}
}
