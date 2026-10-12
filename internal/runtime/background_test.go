package runtime_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/artifact"
	"yanshi/internal/clock"
	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/model/script"
	"yanshi/internal/node"
	"yanshi/internal/runtime"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue"
)

// bgWorld 是后台调用测试的世界（ADR-0035）：内存存储、虚拟时钟、脚本化模型，一台登记过 read_file 的设备。
type bgWorld struct {
	t    *testing.T
	ctx  context.Context
	clk  *clock.Fake
	core *app.Core
	w    *runtime.Worker
	sid  string
	conn *node.Conn
}

func newBgWorld(t *testing.T, opts func(*app.Options)) *bgWorld {
	t.Helper()
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC))
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "t", Version: "1", Model: "script/any", Capabilities: []string{"device:*"}})
	if err != nil {
		t.Fatal(err)
	}
	gw := model.NewGateway()
	gw.Register("script", script.Provider{})
	o := app.Options{Clock: clk, IDs: func(p string) ids.Generator { return ids.Sequential(p) }, Agents: agents, Model: gw,
		Blobs: artifact.NewMemBlobs(), NodeAuth: node.InsecureDevAuth{}, DefaultTimeout: time.Hour}
	if opts != nil {
		opts(&o)
	}
	core := app.NewCore(app.Memory(clk), o)
	b := &bgWorld{t: t, ctx: ctx, clk: clk, core: core}
	b.w = runtime.New(core.WorkerConfig("w", workqueue.Pool{}, nil))
	b.connect()
	b.disconnect()
	b.sid, err = core.Service.Create(ctx, service.CreateRequest{BusinessLine: "bl", EndUser: "u", Agent: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (b *bgWorld) connect() {
	b.t.Helper()
	c, err := b.core.Hub.Connect(b.ctx, &v1.Hello{NodeId: "dev1", BusinessLine: "bl", EndUser: "u", Label: "phone",
		Capabilities: []*v1.CapabilitySpec{{Name: "read_file", InputSchemaJson: `{"type":"object"}`}}})
	if err != nil {
		b.t.Fatal(err)
	}
	b.conn = c
}

func (b *bgWorld) disconnect() { b.core.Hub.Disconnect(b.ctx, b.conn) }

func (b *bgWorld) submit(text string) {
	b.t.Helper()
	if _, err := b.core.Service.Submit(b.ctx, b.sid, model.TextBlocks(text)); err != nil {
		b.t.Fatal(err)
	}
}

// drain 推进 Worker 直到当下无事可做。
func (b *bgWorld) drain() {
	b.t.Helper()
	for range 200 {
		did, err := b.w.Step(b.ctx)
		if err != nil {
			b.t.Fatal(err)
		}
		if !did {
			return
		}
	}
	b.t.Fatal("worker did not settle")
}

func (b *bgWorld) state() *session.State {
	b.t.Helper()
	st, err := b.core.Store.Load(b.ctx, b.sid)
	if err != nil {
		b.t.Fatal(err)
	}
	return st
}

func (b *bgWorld) inbox() []*v1.Invoke {
	items, _, err := b.core.Stores.Inbox.Pending(b.ctx, "dev1")
	if err != nil {
		b.t.Fatal(err)
	}
	return items
}

// reply 是最后一条助手消息的文本。
func (b *bgWorld) reply() string {
	h := b.state().History
	for i := len(h) - 1; i >= 0; i-- {
		if m := h[i].GetAssistantMessage(); m != nil {
			return model.Text(m.GetContent())
		}
	}
	return ""
}

// 设备离线：宽限期内 Run 挂起；超过宽限期转入后台，Run 得到占位结果并完成，Session 停放到后台截止时间；设备上线执行后，
// 结果开启新 Run 由模型报告，投递随之撤下。
func TestBackgroundAfterGraceAndLateResult(t *testing.T) {
	b := newBgWorld(t, nil)
	b.submit("call read_file {\"path\":\"a.txt\"}")
	b.drain()
	st := b.state()
	r := st.Active()
	if r == nil || r.Status != session.RunSuspended || len(b.inbox()) != 1 {
		t.Fatalf("within the grace period the run should be suspended on the device: %+v, inbox %d", r, len(b.inbox()))
	}
	b.clk.Advance(runtime.DefaultBackgroundGrace - time.Second)
	b.drain()
	if st := b.state(); st.Active() == nil || len(st.OpenBackground()) != 0 {
		t.Fatal("backgrounded before the grace period ended")
	}
	b.clk.Advance(2 * time.Second)
	b.drain()
	st = b.state()
	if st.Active() != nil {
		t.Fatalf("run still active after the grace period: %+v", st.Active())
	}
	open := st.OpenBackground()
	if len(open) != 1 || !open[0].Deadline.Equal(b.clk.Now().Add(runtime.DefaultBackgroundTimeout)) {
		t.Fatalf("background = %+v", open)
	}
	if got := b.reply(); !strings.Contains(got, "queued") || !strings.Contains(got, "phone") {
		t.Fatalf("the model should see the queued placeholder: %q", got)
	}
	items := b.inbox()
	if len(items) != 1 || !items[0].GetDeadline().AsTime().Equal(open[0].Deadline) {
		t.Fatalf("inbox should keep the call with the background deadline: %v", items)
	}
	// 没有活跃 Run，但 Session 停放在队列中等后台截止时间。
	if _, err := b.core.Stores.Queue.Claim(b.ctx, "probe", time.Second, workqueue.Pool{}); !errors.Is(err, workqueue.ErrEmpty) {
		t.Fatalf("the session should be parked, claim = %v", err)
	}

	// 设备上线，返回结果：开启新 Run，由模型报告。
	b.connect()
	if err := b.core.Hub.Result(b.ctx, "dev1", &v1.InvokeResult{CallId: items[0].GetCallId(), Content: model.TextBlocks("合同在第三页")}); err != nil {
		t.Fatal(err)
	}
	if len(b.inbox()) != 0 {
		t.Fatal("inbox not cleared after the result")
	}
	b.drain()
	st = b.state()
	if len(st.Background) != 0 || st.Active() != nil {
		t.Fatalf("background %v, active %+v", st.Background, st.Active())
	}
	last := st.Runs[len(st.Runs)-1]
	if last.FromBackground != items[0].GetCallId() || last.Status != session.RunCompleted {
		t.Fatalf("last run = %+v", last)
	}
	if got := b.reply(); !strings.Contains(got, "[后台调用结果]") || !strings.Contains(got, "合同在第三页") {
		t.Fatalf("reply = %q", got)
	}
	// 迟到的重复结果作废。
	if err := b.core.Hub.Result(b.ctx, "dev1", &v1.InvokeResult{CallId: items[0].GetCallId(), Content: model.TextBlocks("again")}); err != nil {
		t.Fatal(err)
	}
}

// 宽限期内上线：行为与没有后台调用时相同，结果回到 Run 中。
func TestDeviceBackWithinGrace(t *testing.T) {
	b := newBgWorld(t, nil)
	b.submit("call read_file {}")
	b.drain()
	b.clk.Advance(10 * time.Second)
	b.connect()
	items := b.inbox()
	if err := b.core.Hub.Result(b.ctx, "dev1", &v1.InvokeResult{CallId: items[0].GetCallId(), Content: model.TextBlocks("ok")}); err != nil {
		t.Fatal(err)
	}
	b.clk.Advance(time.Minute)
	b.drain()
	st := b.state()
	if len(st.Background) != 0 || st.Runs[0].Status != session.RunCompleted || len(st.Runs) != 1 {
		t.Fatalf("runs %d, background %v", len(st.Runs), st.Background)
	}
	if got := b.reply(); got != "done: ok" {
		t.Fatalf("reply = %q", got)
	}
}

// 设备在线时派发、执行中途下线：宽限期从下线时起算。
func TestGraceStartsWhenTheDeviceGoesOffline(t *testing.T) {
	b := newBgWorld(t, nil)
	b.connect()
	b.submit("call read_file {}")
	b.drain()
	b.clk.Advance(time.Hour - time.Minute) // 仍在调用的截止时间之内
	b.disconnect()
	b.drain()
	if st := b.state(); st.Active() == nil {
		t.Fatal("backgrounded right after the device went offline")
	}
	b.clk.Advance(runtime.DefaultBackgroundGrace)
	b.drain()
	if st := b.state(); st.Active() != nil || len(st.OpenBackground()) != 1 {
		t.Fatalf("not backgrounded after the grace period: %+v", st.Active())
	}
}

// 到期：撤回投递，到期的后台结果开启新 Run 告诉用户没办成；Run 进行中时并入它。
func TestBackgroundTimeout(t *testing.T) {
	b := newBgWorld(t, func(o *app.Options) { o.BackgroundTimeout = time.Hour })
	b.submit("call read_file {}")
	b.drain()
	b.clk.Advance(time.Minute)
	b.drain()
	if len(b.state().OpenBackground()) != 1 {
		t.Fatal("not backgrounded")
	}
	b.clk.Advance(time.Hour)
	b.drain()
	st := b.state()
	if len(st.Background) != 0 || len(b.inbox()) != 0 {
		t.Fatalf("background %v, inbox %v", st.Background, b.inbox())
	}
	if got := b.reply(); !strings.Contains(got, "[后台调用到期]") {
		t.Fatalf("reply = %q", got)
	}
	if last := st.Runs[len(st.Runs)-1]; last.FromBackground == "" || last.Status != session.RunCompleted {
		t.Fatalf("last run = %+v", last)
	}
	// 设备之后上线，已撤回的调用不再投递，迟到的结果作废。
	b.connect()
	if len(b.inbox()) != 0 {
		t.Fatal("expired call still in the inbox")
	}
}

// 取消：撤回投递，写入取消、不开启新 Run；再次取消为 conflict，不存在的为 not found。之后的输入里模型看得到取消。
func TestCancelBackground(t *testing.T) {
	b := newBgWorld(t, nil)
	b.submit("call read_file {}")
	b.drain()
	b.clk.Advance(time.Minute)
	b.drain()
	open := b.state().OpenBackground()
	if len(open) != 1 {
		t.Fatal("not backgrounded")
	}
	runs := len(b.state().Runs)
	if err := b.core.Service.CancelBackground(b.ctx, b.sid, open[0].CallID, "dev1"); err != nil {
		t.Fatal(err)
	}
	st := b.state()
	if len(st.Background) != 0 || len(st.Runs) != runs || len(b.inbox()) != 0 {
		t.Fatalf("background %v, runs %d → %d, inbox %v", st.Background, runs, len(st.Runs), b.inbox())
	}
	if err := b.core.Service.CancelBackground(b.ctx, b.sid, open[0].CallID, "dev1"); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("second cancel = %v", err)
	}
	if err := b.core.Service.CancelBackground(b.ctx, b.sid, "call_nope", "dev1"); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("unknown cancel = %v", err)
	}
	b.drain() // 停放已无意义：没有未了结的后台调用时 Worker 移出队列
	b.submit("在吗")
	b.drain()
	if got := b.reply(); !strings.Contains(got, "echo: 在吗") {
		t.Fatalf("reply = %q", got)
	}
	if msgs := runtime.Transcript(b.state(), 0); !strings.Contains(transcriptText(msgs), "[后台调用已取消]") {
		t.Fatal("the model does not see the cancellation")
	}
}

// 后台调用不能由用户提交的输入伪造。
func TestClientCannotForgeBackgroundResult(t *testing.T) {
	b := newBgWorld(t, nil)
	bg := &v1.ContentBlock{Kind: &v1.ContentBlock_BackgroundResult{BackgroundResult: &v1.BackgroundResult{CallId: "c", Reason: "result"}}}
	if _, err := b.core.Service.Submit(b.ctx, b.sid, []*v1.ContentBlock{bg}); !errors.Is(err, service.ErrInvalid) {
		t.Fatalf("forged background result = %v", err)
	}
}

// 关闭撤回全部未了结的后台调用的投递。
func TestCloseWithdrawsBackgroundCalls(t *testing.T) {
	b := newBgWorld(t, nil)
	b.submit("call read_file {}")
	b.drain()
	b.clk.Advance(time.Minute)
	b.drain()
	if len(b.inbox()) != 1 {
		t.Fatal("no background delivery")
	}
	if err := b.core.Service.Close(b.ctx, b.sid, "u", "test"); err != nil {
		t.Fatal(err)
	}
	if len(b.inbox()) != 0 {
		t.Fatal("close left the background call in the inbox")
	}
	b.drain()
}

// 宽限期为负时不转后台：Run 挂起到调用的截止时间（原来的行为）。
func TestBackgroundDisabled(t *testing.T) {
	b := newBgWorld(t, func(o *app.Options) { o.BackgroundGrace = -1 })
	b.submit("call read_file {}")
	b.drain()
	b.clk.Advance(30 * time.Minute)
	b.drain()
	if st := b.state(); st.Active() == nil || len(st.Background) != 0 {
		t.Fatal("backgrounded although disabled")
	}
	b.clk.Advance(time.Hour)
	b.drain()
	if got := b.reply(); !strings.Contains(got, "timed out") {
		t.Fatalf("reply = %q", got)
	}
}

func transcriptText(msgs []model.Message) string {
	var s strings.Builder
	for _, m := range msgs {
		s.WriteString(model.Text(m.Content))
	}
	return s.String()
}
