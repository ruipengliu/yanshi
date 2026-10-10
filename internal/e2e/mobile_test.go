package e2e

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
	yanshi "yanshi/sdk/mobile"
)

// 移动端绑定层（sdk/mobile）的原生侧实现：参数与回调都是 protobuf 字节，与 Swift / Kotlin 看到的一致。

type mobileTokens struct{}

func (mobileTokens) Token() string { return mustSign("u") }

type mobileView struct {
	mu     sync.Mutex
	events []*v1.Event
	viewer []*v1.Viewer
}

func (v *mobileView) OnEvent(b []byte) {
	e := &v1.Event{}
	if err := proto.Unmarshal(b, e); err != nil {
		panic(err)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.events = append(v.events, e)
}

func (v *mobileView) OnDelta([]byte) {}

func (v *mobileView) OnPresence(b []byte) {
	p := &v1.Presence{}
	if err := proto.Unmarshal(b, p); err != nil {
		panic(err)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.viewer = p.GetViewers()
}

func (v *mobileView) OnEnded(string) {}

func (v *mobileView) lastAssistantText() (string, int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	text, done := "", 0
	for _, e := range v.events {
		if m := e.GetAssistantMessage(); m != nil {
			text = model.Text(m.GetContent())
		}
		if e.GetRunCompleted() != nil {
			done++
		}
	}
	return text, done
}

// photoCapability 拍一张"照片"：上传为工件，返回 Media 内容块。
type photoCapability struct{ c **yanshi.Client }

func (p photoCapability) Execute(b []byte) ([]byte, error) {
	inv := &v1.Invoke{}
	if err := proto.Unmarshal(b, inv); err != nil {
		return nil, err
	}
	block, err := (*p.c).UploadArtifact(inv.GetSessionId(), "photo.jpg", "image/jpeg", []byte("jpeg bytes"))
	if err != nil {
		return nil, err
	}
	media := &v1.ContentBlock{}
	if err := proto.Unmarshal(block, media); err != nil {
		return nil, err
	}
	return proto.Marshal(&v1.InvokeResult{CallId: inv.GetCallId(), Content: []*v1.ContentBlock{media}})
}

type failingCapability struct{}

func (failingCapability) Execute([]byte) ([]byte, error) {
	return nil, errors.New("camera unavailable")
}

func mustMarshal(m proto.Message) []byte {
	b, err := proto.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

// TestMobileBinding：移动端绑定层既是 Node（执行原生能力、上传工件）又是会话客户端（以字节收发请求与事件）。
func TestMobileBinding(t *testing.T) {
	e := setup(t)
	connected := make(chan string, 4)
	c := yanshi.NewClient(&yanshi.Config{URL: e.connURL(), NodeID: "iphone-1", Label: "iPhone", Kind: "phone", LedgerDir: t.TempDir()})
	c.SetTokenSource(mobileTokens{})
	c.SetObserver(observer(func(label string) { connected <- label }))
	if err := c.AddCapability([]byte("not a proto \xff"), photoCapability{&c}); err == nil {
		t.Fatal("a malformed capability spec was accepted")
	}
	if err := c.AddCapability(mustMarshal(&v1.CapabilitySpec{Name: "take_photo", Idempotent: true, Risk: v1.Risk_RISK_LOW}), photoCapability{&c}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddCapability(mustMarshal(&v1.CapabilitySpec{Name: "scan", Risk: v1.Risk_RISK_LOW}), failingCapability{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Request(nil, 0); !errors.Is(err, yanshi.ErrNotStarted) {
		t.Fatalf("request before start: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("not connected")
	}
	if err := c.AddCapability(mustMarshal(&v1.CapabilitySpec{Name: "late"}), failingCapability{}); !errors.Is(err, yanshi.ErrStarted) {
		t.Fatalf("capability after start: %v", err)
	}

	request := func(r *v1.ClientRequest) *v1.ClientResponse {
		t.Helper()
		b, err := c.Request(mustMarshal(r), 5000)
		if err != nil {
			t.Fatal(err)
		}
		resp := &v1.ClientResponse{}
		if err := proto.Unmarshal(b, resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	sid := request(&v1.ClientRequest{Op: &v1.ClientRequest_CreateSession{CreateSession: &v1.CreateSession{Agent: "dev"}}}).GetSessionId()
	if sid == "" {
		t.Fatal("no session")
	}
	var view mobileView
	sub, err := c.Subscribe(sid, 0, &view)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	c.SetActivity(sid, true, false)

	// 原生能力上传工件，结果引用它。
	b, err := c.SubmitText(sid, "call take_photo", 5000)
	if err != nil {
		t.Fatal(err)
	}
	if resp := (&v1.ClientResponse{}); proto.Unmarshal(b, resp) != nil || resp.GetRunId() == "" {
		t.Fatalf("submit response = %x", b)
	}
	eventually(t, "photo run completes", 10*time.Second, func() bool { _, n := view.lastAssistantText(); return n == 1 })
	if id := e.lastToolMedia(sid); id == "" {
		t.Fatal("the photo was not uploaded as an artifact of the session")
	}

	// 原生能力返回错误：作为错误结果交给 Agent。
	if _, err := c.SubmitText(sid, "call scan", 5000); err != nil {
		t.Fatal(err)
	}
	eventually(t, "scan run completes", 10*time.Second, func() bool { _, n := view.lastAssistantText(); return n == 2 })
	if text, _ := view.lastAssistantText(); text != "done: [error] camera unavailable" {
		t.Fatalf("assistant saw %q", text)
	}

	// 网关拒绝的请求以响应中的 error 表示，而不是 Go error。
	resp := request(&v1.ClientRequest{Op: &v1.ClientRequest_Decide{Decide: &v1.DecideApproval{SessionId: "nope", CallId: "x", Approve: true}}})
	if resp.GetError().GetCode() != "not_found" {
		t.Fatalf("decide on a missing session = %v", resp)
	}
	view.mu.Lock()
	viewers := view.viewer
	view.mu.Unlock()
	if len(viewers) != 1 || viewers[0].GetDeviceId() != "iphone-1" {
		t.Fatalf("presence = %v", viewers)
	}

	// 停止之后请求立即失败。
	c.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.SubmitText(sid, "hi", 0); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("request after stop succeeded")
		}
	case <-ctx.Done():
		t.Fatal("request after stop hangs")
	}
}

type observer func(string)

func (o observer) OnConnected(label string) { o(label) }

// TestMobileSwift：gomobile 生成的 xcframework 在 Swift 中可用（sdk/mobile/smoke）。冒烟程序由 make mobile 构建，
// YANSHI_TEST_MOBILE_SMOKE 给出运行它的命令：macOS 切片直接是程序路径，iOS 模拟器切片是
// "xcrun simctl spawn <设备> <程序路径>"（模拟器与本机共用网络）。未设置时跳过（需要 Xcode，不在 make check 中）。
func TestMobileSwift(t *testing.T) {
	command := strings.Fields(os.Getenv("YANSHI_TEST_MOBILE_SMOKE"))
	if len(command) == 0 {
		t.Skip("YANSHI_TEST_MOBILE_SMOKE not set")
	}
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	for k, v := range map[string]string{"YANSHI_URL": e.connURL(), "YANSHI_TOKEN": mustSign("u")} {
		// simctl spawn 只把 SIMCTL_CHILD_ 前缀的变量传给模拟器中的进程。
		cmd.Env = append(cmd.Env, k+"="+v, "SIMCTL_CHILD_"+k+"="+v)
	}
	cmd.Env = append(os.Environ(), cmd.Env...)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(out)), "ok") {
		t.Fatalf("swift smoke failed: %v\n%s", err, out)
	}
}
