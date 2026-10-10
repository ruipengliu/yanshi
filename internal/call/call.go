// Package call 中转 Call（全双工语音通话，docs/design/m3-call.md，ADR-0027）：设备经 Connection 送来麦克风音频，
// 这里按实时节奏转给实时语音模型，把模型的语音与转写送回设备；双方每句话的转写经 service 写入日志。
//
// 语音模型只有一个工具 run_task：需要能力的工作交给 Session 的 Agent 作为后台 Run 执行（ADR-0005）。
// 短任务在工具调用挂起期间完成，结果作为工具结果交还模型，由它自然地说出；长任务先回答"在后台进行"，
// 结束后由这里在通话空闲时播报。Run 等待审批或提问时同样告知模型或播报。
//
// Call 的状态只在中转它的进程中：进程退出时 Call 随之结束（日志中留下没有 CallEnded 的 Call，
// 下一个 Call 开始时以 "replaced" 补上）。
package call

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/feed"
	"yanshi/internal/metrics"
	"yanshi/internal/moderation"
	"yanshi/internal/realtime"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/usage"
)

type Manager struct {
	Service  *service.Service
	Realtime *realtime.Gateway
	// Feed 用于跟随 Session 的日志：派生 Run 的进展、Call 被取代或 Session 被关闭。
	Feed  feed.Source
	Meter *usage.Meter
	// HoldFor 是 run_task 挂起等待 Run 结果的时长，默认 20 秒：短任务的结果由模型自然地说出；
	// 超过它先回答"在后台进行"，结束后再播报。实测挂起 30 秒模型仍正常回复。
	HoldFor time.Duration
	// MaxBacklog 是上行音频积压的上限（帧），默认 15（300 ms）：网络抖动后积压的音频超出时丢弃最旧的，
	// 使对话延迟有界。
	MaxBacklog int
	Logger     *slog.Logger
}

func (m *Manager) log() *slog.Logger {
	if m.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return m.Logger
}

func (m *Manager) holdFor() time.Duration {
	if m.HoldFor == 0 {
		return 20 * time.Second
	}
	return m.HoldFor
}

var (
	// ErrNotSupported 表示 Session 的 AgentDef 没有配置通话，或其实时语音模型未配置。
	ErrNotSupported = fmt.Errorf("%w: the agent does not support calls", service.ErrInvalid)
	// ErrProvider 表示无法连接实时语音模型。
	ErrProvider = errors.New("realtime model unavailable")
)

type StartRequest struct {
	SessionID string
	CallID    string
	DeviceID  string
}

// Start 打开实时语音模型、记录 CallStarted，开始中转。out 把消息发给发起的设备（可被并发调用，须自行串行化）；
// 成功时先发出 CallReady。调用方须已确认设备有权访问该 Session。
func (m *Manager) Start(ctx context.Context, req StartRequest, out func(*v1.CallEvent) error) (*Call, error) {
	st, err := m.Service.Load(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
	def, err := m.Service.Agents.Get(st.Agent)
	if err != nil {
		return nil, err
	}
	cfg := def.Call
	if cfg.Model == "" || m.Realtime == nil || !m.Realtime.Has(cfg.Model) {
		return nil, ErrNotSupported
	}
	rt, err := m.Realtime.Open(ctx, cfg.Model, realtime.Config{Instructions: cfg.Instructions, Voice: cfg.Voice,
		Tools: []realtime.Tool{RunTaskTool}, History: RecentTurns(st, maxHistoryTurns)})
	if err != nil {
		m.log().Warn("realtime open failed", "session", req.SessionID, "model", cfg.Model, "err", err)
		return nil, fmt.Errorf("%w: %v", ErrProvider, err)
	}
	if err := m.Service.StartCall(ctx, service.StartCallRequest{SessionID: req.SessionID, CallID: req.CallID,
		DeviceID: req.DeviceID, Model: cfg.Model, Voice: cfg.Voice}); err != nil {
		rt.Close()
		return nil, err
	}
	// 从 CallStarted 之后跟随日志。
	st, err = m.Service.Load(ctx, req.SessionID)
	if err != nil {
		rt.Close()
		return nil, err
	}
	backlog := m.MaxBacklog
	if backlog == 0 {
		backlog = 15
	}
	c := &Call{m: m, sid: req.SessionID, id: req.CallID, model: cfg.Model, agent: def.Name + "@" + def.Version,
		scope: usage.Scope{SessionID: req.SessionID, BusinessLine: st.Created.GetBusinessLine(), EndUser: st.Created.GetEndUser()},
		rt:    rt, out: out, backlog: backlog, writes: make(chan func(), 64), done: make(chan struct{}),
		runs: map[string]*runState{}, watches: map[string]*watch{}, changed: make(chan struct{})}
	c.ctx, c.cancel = context.WithCancel(context.WithoutCancel(ctx))
	metrics.CallsActive.Inc()
	c.send(&v1.CallEvent{Kind: &v1.CallEvent_Ready{Ready: &v1.CallReady{Model: cfg.Model, Voice: cfg.Voice,
		InputSampleRate: realtime.InputSampleRate, OutputSampleRate: realtime.OutputSampleRate}}})
	c.wg.Add(4)
	go c.pace()
	go c.listen()
	go c.writer()
	go c.follow(st)
	return c, nil
}

const maxHistoryTurns = 20

// RecentTurns 是放入语音模型上下文的近期对话：用户的输入、助手的回复与之前通话中的话（不含调用与结果），
// 每句至多 300 字。之前的对话被压缩过时，以摘要开头。
func RecentTurns(st *session.State, n int) []realtime.Turn {
	clip := func(s string) string {
		s = strings.TrimSpace(s)
		if utf8.RuneCountInString(s) > 300 {
			s = string([]rune(s)[:300]) + "……"
		}
		return s
	}
	var turns []realtime.Turn
	add := func(role, text string) {
		if text = clip(text); text != "" {
			turns = append(turns, realtime.Turn{Role: role, Text: text})
		}
	}
	for _, e := range st.History {
		switch p := e.GetPayload().(type) {
		case *v1.Event_RunRequested:
			if p.RunRequested.GetFromCall() == "" {
				add("user", textOf(p.RunRequested.GetInput()))
			}
		case *v1.Event_Steered:
			if p.Steered.GetFromCall() == "" {
				add("user", textOf(p.Steered.GetInput()))
			}
		case *v1.Event_AssistantMessage:
			add("assistant", textOf(p.AssistantMessage.GetContent()))
		case *v1.Event_CallTranscript:
			add(p.CallTranscript.GetRole(), p.CallTranscript.GetText())
		}
	}
	if len(turns) > n {
		turns = turns[len(turns)-n:]
	}
	if c := st.Compaction; c != nil {
		turns = append([]realtime.Turn{{Role: "user", Text: "（之前对话的摘要）" + clip(textOf(c.GetSummary()))}}, turns...)
	}
	return turns
}

func textOf(blocks []*v1.ContentBlock) string {
	var b strings.Builder
	for _, x := range blocks {
		b.WriteString(x.GetText().GetText())
	}
	return b.String()
}

// Call 是一个进行中的 Call。
type Call struct {
	m       *Manager
	sid, id string
	model   string
	agent   string
	scope   usage.Scope
	rt      realtime.Session
	out     func(*v1.CallEvent) error
	backlog int

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	writes chan func()
	done   chan struct{}
	once   sync.Once
	turns  int

	mu sync.Mutex
	// buf 是尚未转发的上行音频。
	buf   []byte
	muted bool
	// resp 是正在进行的回复；toolsPending 是挂起的工具调用数；speak 是等通话空闲时播报的内容。
	resp         *response
	toolsPending int
	speak        []string
	// runs 是跟随日志得到的、本 Call 开始之后各 Run 的进展；watches 是派生任务所在的 Run。
	runs    map[string]*runState
	watches map[string]*watch
	// changed 在 runs 更新时关闭并换新，唤醒等待的 watch。
	changed chan struct{}
	ended   bool
}

type response struct {
	id   string
	text strings.Builder
	// logged 表示已写入（整句，或被打断时的部分）。
	logged bool
}

// ID 返回 Call ID。
func (c *Call) ID() string { return c.id }

// Done 在 Call 结束后关闭。
func (c *Call) Done() <-chan struct{} { return c.done }

func (c *Call) send(e *v1.CallEvent) {
	c.mu.Lock()
	ended := c.ended
	c.mu.Unlock()
	if ended {
		return
	}
	e.CallId = c.id
	if err := c.out(e); err != nil {
		c.End(service.CallEndDisconnected)
	}
}

// Audio 接收设备的麦克风音频（PCM 16 位、16 kHz）。积压超过上限时丢弃最旧的。
func (c *Call) Audio(pcm []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.muted || c.ended {
		return
	}
	c.buf = append(c.buf, pcm...)
	if max := c.backlog * realtime.FrameBytes; len(c.buf) > max {
		drop := len(c.buf) - max
		drop += (realtime.FrameBytes - drop%realtime.FrameBytes) % realtime.FrameBytes
		metrics.CallAudioDropped.WithLabelValues().Add(float64(drop / realtime.FrameBytes))
		c.buf = append([]byte(nil), c.buf[drop:]...)
	}
}

// Control 处理设备的控制消息。
func (c *Call) Control(a v1.CallAction) {
	switch a {
	case v1.CallAction_CALL_ACTION_MUTE, v1.CallAction_CALL_ACTION_UNMUTE:
		muted := a == v1.CallAction_CALL_ACTION_MUTE
		c.mu.Lock()
		c.muted, c.buf = muted, nil
		c.mu.Unlock()
		if err := c.rt.Mute(muted); err != nil {
			c.End(service.CallEndProvider)
		}
	case v1.CallAction_CALL_ACTION_INTERRUPT:
		c.mu.Lock()
		c.flushResponseLocked()
		c.mu.Unlock()
		if err := c.rt.CancelResponse(); err != nil {
			c.End(service.CallEndProvider)
		}
	case v1.CallAction_CALL_ACTION_HANGUP:
		c.End(service.CallEndHangup)
	}
}

// End 结束 Call：关闭语音模型会话，记录 CallEnded，通知设备。可重复调用。
func (c *Call) End(reason string) { c.end(reason, true) }

// end 的 record 为 false 时不写 CallEnded：Call 已在日志中结束（被取代、Session 已关闭或删除）。
func (c *Call) end(reason string, record bool) {
	c.once.Do(func() {
		c.mu.Lock()
		c.ended = true
		c.mu.Unlock()
		c.cancel()
		c.rt.Close()
		metrics.CallsActive.Dec()
		metrics.CallsEnded.WithLabelValues(reason).Inc()
		if record {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := c.m.Service.EndCall(ctx, c.sid, c.id, reason); err != nil && !errors.Is(err, service.ErrNotFound) {
				c.m.log().Warn("call end not recorded", "session", c.sid, "call", c.id, "err", err)
			}
			cancel()
		}
		_ = c.out(&v1.CallEvent{CallId: c.id, Kind: &v1.CallEvent_Ended{Ended: &v1.CallEnded{CallId: c.id, Reason: reason}}})
		go func() {
			c.wg.Wait()
			close(c.done)
		}()
	})
}

// pace 按 20 ms 的节拍把上行音频转给模型；没有音频时发静音：模型依赖连续的音频流（实测节奏偏离会报错）。
func (c *Call) pace() {
	defer c.wg.Done()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	silence := make([]byte, realtime.FrameBytes)
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-tick.C:
		}
		c.mu.Lock()
		if c.muted {
			c.mu.Unlock()
			continue
		}
		frame := silence
		if len(c.buf) >= realtime.FrameBytes {
			frame = c.buf[:realtime.FrameBytes:realtime.FrameBytes]
			c.buf = c.buf[realtime.FrameBytes:]
		}
		c.mu.Unlock()
		if err := c.rt.Audio(frame); err != nil {
			if c.ctx.Err() == nil {
				c.m.log().Warn("realtime audio failed", "call", c.id, "err", err)
				c.End(service.CallEndProvider)
			}
			return
		}
	}
}

// listen 处理模型的事件。
func (c *Call) listen() {
	defer c.wg.Done()
	for {
		var ev realtime.Event
		var ok bool
		select {
		case <-c.ctx.Done():
			return
		case ev, ok = <-c.rt.Events():
		}
		if !ok {
			c.End(service.CallEndProvider)
			return
		}
		switch ev.Kind {
		case realtime.SpeechStarted:
			// 用户开口：尚未生成完的回复被打断，已生成的部分现在就写入（在用户这句话之前）。
			c.mu.Lock()
			c.flushResponseLocked()
			c.mu.Unlock()
			c.send(&v1.CallEvent{Kind: &v1.CallEvent_Speech{Speech: &v1.CallSpeech{}}})
		case realtime.UserTranscript:
			c.send(&v1.CallEvent{Kind: &v1.CallEvent_Text{Text: &v1.CallText{Role: "user", Text: ev.Text, Final: ev.Final}}})
			if ev.Final {
				c.record("user", ev.Text, false)
			}
		case realtime.AssistantText:
			c.assistantText(ev)
		case realtime.AssistantAudio:
			c.send(&v1.CallEvent{Kind: &v1.CallEvent_Audio{Audio: &v1.CallOutputAudio{Pcm: ev.Audio, ResponseId: ev.ResponseID}}})
		case realtime.ResponseDone:
			c.responseDone(ev.ResponseID)
		case realtime.ToolCall:
			c.toolCall(ev)
		case realtime.UsageReport:
			c.turns++
			c.meter(ev.Usage, fmt.Sprintf("%s/%d", c.id, c.turns))
		case realtime.Failed:
			c.m.log().Warn("realtime session failed", "session", c.sid, "call", c.id, "err", ev.Err)
			c.End(service.CallEndProvider)
			return
		}
	}
}

func (c *Call) assistantText(ev realtime.Event) {
	c.mu.Lock()
	if c.resp != nil && c.resp.id == sayID {
		// 播报产生的回复：已由 speakNow 写入，这里只认领它的 ID。
		c.resp.id = ev.ResponseID
	}
	if c.resp == nil || c.resp.id != ev.ResponseID {
		c.flushResponseLocked()
		c.resp = &response{id: ev.ResponseID}
	}
	r := c.resp
	// 整句在语音播完之前就已生成：此时检查并写入，违规时还来得及中止播放。已作为被打断的部分写入过的回复
	// （用户开口之后模型仍说完了）不再重复写入。
	record := false
	if ev.Final {
		r.text.Reset()
		r.text.WriteString(ev.Text)
		record, r.logged = !r.logged, true
		if record {
			c.recordLocked("assistant", ev.Text, false)
		}
	} else {
		r.text.WriteString(ev.Text)
	}
	c.mu.Unlock()
	c.send(&v1.CallEvent{Kind: &v1.CallEvent_Text{Text: &v1.CallText{Role: "assistant", Text: ev.Text, Final: ev.Final, ResponseId: ev.ResponseID}}})
}

// flushResponseLocked 写入未完成（被打断）的回复已生成的部分。须持有 c.mu。
func (c *Call) flushResponseLocked() {
	if r := c.resp; r != nil && !r.logged {
		r.logged = true
		if text := r.text.String(); text != "" {
			c.recordLocked("assistant", text, true)
		}
	}
}

func (c *Call) responseDone(id string) {
	c.mu.Lock()
	if c.resp != nil && (id == "" || c.resp.id == id || c.resp.id == sayID) {
		c.flushResponseLocked()
		c.resp = nil
	}
	c.mu.Unlock()
	c.send(&v1.CallEvent{Kind: &v1.CallEvent_ResponseDone{ResponseDone: &v1.CallResponseDone{ResponseId: id}}})
	c.flushSpeech()
}

// record 把一句话交给写入协程（按顺序写日志）。
func (c *Call) record(role, text string, interrupted bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordLocked(role, text, interrupted)
}

func (c *Call) recordLocked(role, text string, interrupted bool) {
	if c.ended {
		return
	}
	t := &v1.CallTranscript{CallId: c.id, Role: role, Text: text, Interrupted: interrupted}
	select {
	case c.writes <- func() { c.write(t) }:
	default:
		// 写入跟不上（存储或内容安全服务过慢）：宁可挂断，也不让未经检查的对话继续。
		go c.End(service.CallEndError)
	}
}

func (c *Call) writer() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case f := <-c.writes:
			f()
		}
	}
}

// write 写入一句转写。违规（或无法检查）时中止回复并停止播放；Call 已不在进行中时挂断。
func (c *Call) write(t *v1.CallTranscript) {
	err := c.m.Service.AppendTranscript(c.ctx, c.sid, t)
	switch {
	case err == nil:
	case errors.Is(err, moderation.ErrRejected), errors.Is(err, moderation.ErrUnavailable):
		metrics.CallTranscriptsBlocked.WithLabelValues(t.GetRole()).Inc()
		c.m.log().Info("call transcript blocked", "session", c.sid, "call", c.id, "role", t.GetRole(), "err", err)
		_ = c.rt.CancelResponse()
		c.send(&v1.CallEvent{Kind: &v1.CallEvent_Speech{Speech: &v1.CallSpeech{}}})
		if t.GetRole() == "user" {
			c.speakNow(moderation.Refusal)
		}
	case errors.Is(err, service.ErrConflict), errors.Is(err, service.ErrNotFound):
		c.end(c.endedReason(), false)
	case c.ctx.Err() != nil:
	default:
		c.m.log().Warn("call transcript not recorded", "session", c.sid, "call", c.id, "err", err)
		c.End(service.CallEndError)
	}
}

// endedReason 判断 Call 为何已不在进行中。
func (c *Call) endedReason() string {
	st, err := c.m.Service.Load(context.Background(), c.sid)
	switch {
	case err != nil:
		return service.CallEndSessionClosed
	case st.Closed != nil:
		return service.CallEndSessionClosed
	}
	return service.CallEndReplaced
}

func (c *Call) meter(u realtime.Usage, id string) {
	if c.m.Meter != nil {
		c.m.Meter.Call(c.ctx, c.scope, id, c.agent, c.model, usage.CallTokens{InputText: u.InputText, InputAudio: u.InputAudio,
			CachedInput: u.CachedInput, OutputText: u.OutputText, OutputAudio: u.OutputAudio}, time.Now())
	}
	// 配额在通话中用尽时挂断（与 Run 在下一次模型调用前检查相同）。
	q := c.m.Service.Quotas
	if q.Enabled(c.scope.BusinessLine) {
		p, err := q.Check(c.ctx, c.scope.BusinessLine, c.scope.EndUser, time.Now())
		if err == nil && p != nil {
			c.End(service.CallEndQuota)
		}
	}
}

// sayID 是播报占位回复的 ID：播报产生的回复 ID 事先不知道，它的第一条文本认领真实 ID，或任何结束都算它的。
// 占位期间视为忙，避免两次播报重叠。
const sayID = "say"

// speakNow 立即让模型说出 text，并把它记为助手的话（之前未完成的回复按被打断写入）。
func (c *Call) speakNow(text string) {
	c.mu.Lock()
	c.flushResponseLocked()
	c.resp = &response{id: sayID, logged: true}
	c.recordLocked("assistant", text, false)
	c.mu.Unlock()
	if err := c.rt.Say(text); err != nil {
		c.End(service.CallEndProvider)
	}
}

// enqueueSpeech 在通话空闲（没有进行中的回复与挂起的工具调用）时播报 text；忙时排队。
func (c *Call) enqueueSpeech(text string) {
	c.mu.Lock()
	c.speak = append(c.speak, text)
	c.mu.Unlock()
	c.flushSpeech()
}

func (c *Call) flushSpeech() {
	c.mu.Lock()
	if c.resp != nil || c.toolsPending > 0 || len(c.speak) == 0 || c.ended {
		c.mu.Unlock()
		return
	}
	text := c.speak[0]
	c.speak = c.speak[1:]
	c.resp = &response{id: sayID, logged: true}
	c.mu.Unlock()
	// 先补进模型上下文，之后用户追问时模型知道说过什么。
	_ = c.rt.AddContext([]realtime.Turn{{Role: "user", Text: "（后台任务有了新进展）"}, {Role: "assistant", Text: text}})
	c.speakNow(text)
}

// toolCall 处理模型的函数调用。
func (c *Call) toolCall(ev realtime.Event) {
	if ev.Name != RunTask {
		_ = c.rt.ToolResult(ev.CallID, fmt.Sprintf("没有名为 %s 的工具；需要做事时用 %s。", ev.Name, RunTask))
		return
	}
	var args struct {
		Task string `json:"task"`
	}
	if err := json.Unmarshal([]byte(ev.Arguments), &args); err != nil || strings.TrimSpace(args.Task) == "" {
		_ = c.rt.ToolResult(ev.CallID, "参数错误：task 不能为空，请写清楚要做什么。")
		return
	}
	c.mu.Lock()
	c.toolsPending++
	c.mu.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.task(ev.CallID, args.Task)
	}()
}

// toolResult 回传工具结果，结束一次挂起。
func (c *Call) toolResult(toolCallID, text string) {
	c.mu.Lock()
	c.toolsPending--
	c.mu.Unlock()
	if err := c.rt.ToolResult(toolCallID, text); err != nil && c.ctx.Err() == nil {
		c.End(service.CallEndProvider)
	}
	c.flushSpeech()
}

// task 把任务交给 Session 的 Agent，并跟随它所在的 Run。
func (c *Call) task(toolCallID, text string) {
	res, err := c.m.Service.SubmitFromCall(c.ctx, c.sid, c.id, c.id+"/"+toolCallID, []*v1.ContentBlock{
		{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: text}}}})
	var exceeded *usage.ExceededError
	switch {
	case err == nil:
	case errors.Is(err, moderation.ErrRejected):
		c.toolResult(toolCallID, "这个任务没有通过内容安全检查，不能执行。请告诉用户换个说法。")
		return
	case errors.As(err, &exceeded):
		c.toolResult(toolCallID, "用户今天的额度已经用完，任务不能执行。请告诉用户。")
		return
	case errors.Is(err, service.ErrConflict), errors.Is(err, service.ErrNotFound):
		c.toolResult(toolCallID, "任务没有提交成功。")
		c.end(c.endedReason(), false)
		return
	default:
		if c.ctx.Err() == nil {
			c.m.log().Warn("call task submit failed", "session", c.sid, "call", c.id, "err", err)
			c.toolResult(toolCallID, "任务暂时提交不了，请告诉用户稍后再试。")
		}
		return
	}
	status := "started"
	switch {
	case res.Answered != "":
		status = "answered"
	case res.Steered:
		status = "steered"
	}
	if res.Answered != "" {
		c.answered(res.RunID, res.Answered)
	}
	c.send(&v1.CallEvent{Kind: &v1.CallEvent_Task{Task: &v1.CallTask{RunId: res.RunID, Status: status}}})
	c.watchRun(res.RunID, toolCallID)
}
