package e2e

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/realtime/fake"
	"yanshi/internal/session"
)

// callClient 是直接说 Connection 协议的会话客户端（Go SDK 的 Call 接口尚未提供）。
type callClient struct {
	t  *testing.T
	ws *websocket.Conn

	mu     sync.Mutex
	events []*v1.CallEvent
	resps  map[string]*v1.ClientResponse
	n      int
}

func (e *env) dialCall(device string) *callClient {
	e.t.Helper()
	ctx := context.Background()
	ws, _, err := websocket.Dial(ctx, e.connURL(), nil)
	if err != nil {
		e.t.Fatal(err)
	}
	ws.SetReadLimit(1 << 20)
	c := &callClient{t: e.t, ws: ws, resps: map[string]*v1.ClientResponse{}}
	e.t.Cleanup(func() { ws.CloseNow() })
	c.send(&v1.NodeMessage{Msg: &v1.NodeMessage_Hello{Hello: &v1.Hello{NodeId: device, Token: mustSign("u"), ClientOnly: true, Label: device}}})
	if m := c.read(ctx); m.GetWelcome() == nil {
		e.t.Fatalf("expected welcome, got %v", m)
	}
	go func() {
		for {
			m := c.read(context.Background())
			if m == nil {
				return
			}
			c.mu.Lock()
			if ce := m.GetCallEvent(); ce != nil {
				c.events = append(c.events, ce)
			}
			if r := m.GetResponse(); r != nil {
				c.resps[r.GetRequestId()] = r
			}
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *callClient) send(m *v1.NodeMessage) {
	b, err := proto.Marshal(m)
	if err != nil {
		c.t.Fatal(err)
	}
	_ = c.ws.Write(context.Background(), websocket.MessageBinary, b)
}

func (c *callClient) read(ctx context.Context) *v1.GatewayMessage {
	_, b, err := c.ws.Read(ctx)
	if err != nil {
		return nil
	}
	m := &v1.GatewayMessage{}
	if err := proto.Unmarshal(b, m); err != nil {
		c.t.Error(err)
		return nil
	}
	return m
}

func (c *callClient) request(op *v1.ClientRequest) *v1.ClientResponse {
	c.t.Helper()
	c.mu.Lock()
	c.n++
	op.RequestId = fmt.Sprint("r", c.n)
	c.mu.Unlock()
	c.send(&v1.NodeMessage{Msg: &v1.NodeMessage_Request{Request: op}})
	var resp *v1.ClientResponse
	eventually(c.t, "response "+op.RequestId, 5*time.Second, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		resp = c.resps[op.RequestId]
		return resp != nil
	})
	return resp
}

func (c *callClient) createSession() string {
	return c.request(&v1.ClientRequest{Op: &v1.ClientRequest_CreateSession{CreateSession: &v1.CreateSession{Agent: "dev"}}}).GetSessionId()
}

func (c *callClient) start(sid, id string) {
	c.send(&v1.NodeMessage{Msg: &v1.NodeMessage_CallStart{CallStart: &v1.CallStart{CallId: id, SessionId: sid}}})
}

func (c *callClient) say(id, text string) {
	c.send(&v1.NodeMessage{Msg: &v1.NodeMessage_CallAudio{CallAudio: &v1.CallAudio{CallId: id, Pcm: fake.Utterance(text)}}})
}

func (c *callClient) control(id string, a v1.CallAction) {
	c.send(&v1.NodeMessage{Msg: &v1.NodeMessage_CallControl{CallControl: &v1.CallControl{CallId: id, Action: a}}})
}

// waitFor 等到出现满足条件的 Call 消息并返回它。
func (c *callClient) waitFor(what string, cond func(*v1.CallEvent) bool) *v1.CallEvent {
	c.t.Helper()
	var found *v1.CallEvent
	eventually(c.t, what, 10*time.Second, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, e := range c.events {
			if cond(e) {
				found = e
				return true
			}
		}
		return false
	})
	return found
}

func (c *callClient) count(cond func(*v1.CallEvent) bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.events {
		if cond(e) {
			n++
		}
	}
	return n
}

func isTask(status string) func(*v1.CallEvent) bool {
	return func(e *v1.CallEvent) bool { return e.GetTask().GetStatus() == status }
}

func isEnded(reason string) func(*v1.CallEvent) bool {
	return func(e *v1.CallEvent) bool { return e.GetEnded() != nil && e.GetEnded().GetReason() == reason }
}

// callLog 返回日志中与 Call 相关的事件，形如 "start"、"user:你好"、"assistant:你说：你好"、"run:<from_call>"、"end:hangup"。
func (e *env) callLog(st stores, sid string) []string {
	e.t.Helper()
	events, err := eventlog.ReadAll(context.Background(), st.log, sid, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, ev := range events {
		switch p := ev.GetPayload().(type) {
		case *v1.Event_CallStarted:
			out = append(out, "start")
		case *v1.Event_CallTranscript:
			s := p.CallTranscript.GetRole() + ":" + p.CallTranscript.GetText()
			if p.CallTranscript.GetInterrupted() {
				s += "(interrupted)"
			}
			out = append(out, s)
		case *v1.Event_RunRequested:
			out = append(out, "run:"+p.RunRequested.GetFromCall())
		case *v1.Event_CallEnded:
			out = append(out, "end:"+p.CallEnded.GetReason())
		}
	}
	return out
}

func callEnv(t *testing.T, hold time.Duration) (*env, stores) {
	st := memStores()
	st.holdFor = hold
	return &env{t: t, srv: instance(t, st, 2, true), disk: map[string][]byte{}}, st
}

// TestCallRelay：一次通话中，双方的话经网关中转并写入日志；派生的短任务在挂起期间完成，结果作为工具结果交给模型；
// 用户开口打断长回复；挂断后日志记下结束。
func TestCallRelay(t *testing.T) {
	e, st := callEnv(t, 5*time.Second)
	phone := e.dialCall("phone")
	sid := phone.createSession()
	phone.start(sid, "vc1")
	ready := phone.waitFor("ready", func(ev *v1.CallEvent) bool { return ev.GetReady() != nil }).GetReady()
	if ready.GetModel() != "fake/voice" || ready.GetInputSampleRate() != 16000 || ready.GetOutputSampleRate() != 24000 {
		t.Fatalf("ready = %v", ready)
	}
	voice := st.voice.Sessions()[0]
	if voice.Config.Instructions != "你是语音助手" || len(voice.Config.Tools) != 1 || voice.Config.Tools[0].Name != "run_task" {
		t.Fatalf("voice config = %+v", voice.Config)
	}

	phone.say("vc1", "你好")
	phone.waitFor("assistant reply", func(ev *v1.CallEvent) bool {
		return ev.GetText().GetFinal() && ev.GetText().GetText() == "你说：你好"
	})
	phone.waitFor("audio", func(ev *v1.CallEvent) bool { return len(ev.GetAudio().GetPcm()) > 0 })

	// 短任务：Run 在挂起期间完成，模型拿到结果再说。
	phone.say("vc1", "task stream 3")
	phone.waitFor("task completed", isTask("completed"))
	phone.waitFor("result spoken", func(ev *v1.CallEvent) bool {
		return ev.GetText().GetFinal() && strings.Contains(ev.GetText().GetText(), "后台助手的回复：streamed")
	})

	// 打断：长回复进行中用户开口。
	phone.say("vc1", "long 100")
	eventually(t, "long reply under way", 10*time.Second, func() bool {
		return phone.count(func(e *v1.CallEvent) bool { return e.GetText().GetText() == "嗯" }) > 3
	})
	phone.say("vc1", "停")
	phone.waitFor("reply to the interruption", func(ev *v1.CallEvent) bool {
		return ev.GetText().GetFinal() && ev.GetText().GetText() == "你说：停"
	})

	phone.control("vc1", v1.CallAction_CALL_ACTION_HANGUP)
	phone.waitFor("ended", isEnded("hangup"))
	if phone.count(func(ev *v1.CallEvent) bool { return ev.GetSpeech() != nil }) < 4 {
		t.Fatal("speech-started events were not relayed")
	}

	var got []string
	eventually(t, "call logged", 5*time.Second, func() bool {
		got = e.callLog(st, sid)
		return len(got) > 0 && got[len(got)-1] == "end:hangup"
	})
	want := []string{"start", "user:你好", "assistant:你说：你好", "user:task stream 3", "run:vc1",
		"assistant:好的。任务完成。后台助手的回复：streamed", "user:long 100", "assistant:嗯…(interrupted)",
		"user:停", "assistant:你说：停", "end:hangup"}
	// 被打断的回复只写入已生成的部分，长度取决于打断的时机。
	ok := len(got) == len(want)
	for i := 0; ok && i < len(want); i++ {
		if want[i] == "assistant:嗯…(interrupted)" {
			ok = strings.HasPrefix(got[i], "assistant:嗯") && strings.HasSuffix(got[i], "嗯(interrupted)") && len([]rune(got[i])) < len([]rune("assistant:"))+100+len("(interrupted)")
		} else {
			ok = got[i] == want[i]
		}
	}
	if !ok {
		t.Fatalf("log:\n got  %v\n want %v", got, want)
	}
	// 后续的文字对话能看到通话内容：之后开始的 Call 以它们为上下文。
	phone.start(sid, "vc2")
	phone.waitFor("second call ready", func(ev *v1.CallEvent) bool { return ev.GetCallId() == "vc2" && ev.GetReady() != nil })
	second := st.voice.Sessions()[1]
	var ctx []string
	for _, turn := range second.Config.History {
		ctx = append(ctx, turn.Role+":"+turn.Text)
	}
	if !strings.Contains(strings.Join(ctx, "|"), "user:你好|assistant:你说：你好") || !strings.Contains(strings.Join(ctx, "|"), "assistant:streamed") {
		t.Fatalf("second call context = %v", ctx)
	}
}

// TestCallLongTaskAndApproval：任务超过挂起时长时先回答"在后台进行"，结束后在通话空闲时播报；
// Run 等待审批时告知模型，批准后的结果再播报。
func TestCallLongTaskAndApproval(t *testing.T) {
	e, st := callEnv(t, 300*time.Millisecond)
	stop := e.startNode(st.ledger)
	defer stop()
	phone := e.dialCall("phone")
	sid := phone.createSession()
	phone.start(sid, "vc1")
	phone.waitFor("ready", func(ev *v1.CallEvent) bool { return ev.GetReady() != nil })
	voice := st.voice.Sessions()[0]

	phone.say("vc1", "task stream 40")
	phone.waitFor("told it runs in background", func(ev *v1.CallEvent) bool {
		return ev.GetText().GetFinal() && strings.Contains(ev.GetText().GetText(), "正在后台进行")
	})
	phone.waitFor("completed", isTask("completed"))
	eventually(t, "result announced", 5*time.Second, func() bool {
		said, ctx, _, _, _ := voice.Snapshot()
		return len(said) == 1 && said[0] == "streamed" && len(ctx) >= 2 && ctx[len(ctx)-1].Text == "streamed"
	})

	phone.say("vc1", "task call macbook__write_file")
	t.Cleanup(func() {
		if t.Failed() {
			events, _ := eventlog.ReadAll(context.Background(), st.log, sid, 0)
			for _, ev := range events {
				t.Logf("%d %v", ev.GetSeq(), ev.GetPayload())
			}
		}
	})
	phone.waitFor("waiting for approval", isTask("waiting"))
	phone.waitFor("asked to approve", func(ev *v1.CallEvent) bool {
		return ev.GetText().GetFinal() && strings.Contains(ev.GetText().GetText(), "需要用户批准")
	})
	events, err := eventlog.ReadAll(context.Background(), st.log, sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	st2, err := session.Reduce(events)
	if err != nil {
		t.Fatal(err)
	}
	callID := ""
	for _, c := range st2.Active().Calls {
		if c.AwaitingApproval() {
			callID = c.Call.GetCallId()
		}
	}
	if r := phone.request(&v1.ClientRequest{Op: &v1.ClientRequest_Decide{Decide: &v1.DecideApproval{SessionId: sid, CallId: callID, Approve: true}}}); r.GetError() != nil {
		t.Fatalf("decide: %v", r.GetError())
	}
	eventually(t, "approved result announced", 10*time.Second, func() bool {
		said, _, _, _, _ := voice.Snapshot()
		return len(said) == 2 && strings.HasPrefix(said[1], "done: written")
	})
	if e.writes.Load() != 1 {
		t.Fatalf("write_file executed %d times", e.writes.Load())
	}
}

// TestCallEndsWhenReplacedOrClosed：同一 Session 在另一台设备上开始新的 Call 时，旧的以 "replaced" 结束；
// 关闭 Session 时进行中的 Call 以 "session_closed" 结束；不支持通话的请求得到错误。
func TestCallEndsWhenReplacedOrClosed(t *testing.T) {
	e, st := callEnv(t, 0)
	phone, pc := e.dialCall("phone"), e.dialCall("pc")
	sid := phone.createSession()
	phone.start(sid, "a")
	phone.waitFor("phone ready", func(ev *v1.CallEvent) bool { return ev.GetReady() != nil })
	pc.start(sid, "b")
	pc.waitFor("pc ready", func(ev *v1.CallEvent) bool { return ev.GetReady() != nil })
	phone.waitFor("phone call replaced", isEnded("replaced"))
	if !st.voice.Sessions()[0].Closed {
		t.Fatal("the replaced call's model session is still open")
	}

	pc.request(&v1.ClientRequest{Op: &v1.ClientRequest_Close{Close: &v1.CloseSession{SessionId: sid}}})
	pc.waitFor("pc call ended with the session", isEnded("session_closed"))

	// 别人的 Session、已关闭的 Session、同一连接上的第二个 Call 都被拒绝。
	other := e.dialCall("x")
	other.start(sid, "c")
	other.waitFor("closed session rejected", func(ev *v1.CallEvent) bool { return ev.GetError().GetCode() == "conflict" })
	sid2 := other.createSession()
	other.start(sid2, "d")
	other.waitFor("ready", func(ev *v1.CallEvent) bool { return ev.GetCallId() == "d" && ev.GetReady() != nil })
	other.start(sid2, "e")
	other.waitFor("second call on a connection rejected", func(ev *v1.CallEvent) bool {
		return ev.GetCallId() == "e" && ev.GetError().GetCode() == "conflict"
	})
	got := e.callLog(st, sid)
	if strings.Join(got, "|") != "start|end:replaced|start|end:session_closed" {
		t.Fatalf("log = %v", got)
	}
}

// TestCallEndsWhenDeviceDisconnects：设备断开连接时 Call 以 "disconnected" 结束。
func TestCallEndsWhenDeviceDisconnects(t *testing.T) {
	e, st := callEnv(t, 0)
	phone := e.dialCall("phone")
	sid := phone.createSession()
	phone.start(sid, "a")
	phone.waitFor("ready", func(ev *v1.CallEvent) bool { return ev.GetReady() != nil })
	phone.ws.CloseNow()
	eventually(t, "call ended", 5*time.Second, func() bool {
		got := e.callLog(st, sid)
		return len(got) == 2 && got[1] == "end:disconnected"
	})
}
