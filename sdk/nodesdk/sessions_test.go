package nodesdk

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
)

type nopHandler struct{}

func (nopHandler) OnEvent(*v1.Event)     {}
func (nopHandler) OnDelta(*v1.LiveDelta) {}
func (nopHandler) OnEnded(string)        {}

// 请求的写入卡住（网络黑洞）时：请求在自己的期限到时返回，其他会话操作不被挡住。
func TestBlockedWriteDoesNotBlockTheClient(t *testing.T) {
	c := NewClient(Config{})
	unblock := make(chan struct{})
	defer close(unblock)
	send := func(ctx context.Context, m *v1.NodeMessage) error {
		if m.GetRequest() == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-unblock:
			return errors.New("connection closed")
		}
	}
	if err := c.sessions.attach(send); err != nil {
		t.Fatal(err)
	}

	stuck := make(chan error, 1)
	go func() {
		_, err := c.Request(context.Background(), &v1.ClientRequest{Op: &v1.ClientRequest_CreateSession{CreateSession: &v1.CreateSession{Agent: "a"}}})
		stuck <- err
	}()
	time.Sleep(20 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		c.Subscribe("s1", 0, nopHandler{})
		c.SetActivity("s1", true, false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("subscribe blocked behind a stuck request write")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Request(ctx, &v1.ClientRequest{Op: &v1.ClientRequest_CreateSession{CreateSession: &v1.CreateSession{Agent: "a"}}})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("request with a stuck write returned %v after %s", err, time.Since(start))
	}
	// 已结束的 ctx：请求不发出。
	if _, err := c.Request(ctx, &v1.ClientRequest{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired request: %v", err)
	}
	unblock <- struct{}{}
	if err := <-stuck; !errors.Is(err, ErrDisconnected) {
		t.Fatalf("write failure reported as %v, want ErrDisconnected", err)
	}
}
