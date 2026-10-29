package delivery

import (
	"context"
	"errors"
	"sync"

	"xinli/task"
)

// PushMessage 不提供正文槽位；供应商只能收到种类、ID 与深链。
type PushMessage struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	SessionID string `json:"session_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	DeepLink  string `json:"deep_link"`
}

type ChannelMessage struct {
	ID       string
	Text     string
	DeepLink string
}

type Push interface {
	Push(context.Context, task.Owner, PushMessage) error
}
type Channel interface {
	Send(context.Context, task.Owner, string, ChannelMessage) error
}

var ErrUnknown = errors.New("delivery: send result unknown")
var ErrChannelApproval = errors.New("delivery: approval requires app")

type Sent struct {
	Owner   task.Owner
	Channel string
	Push    *PushMessage
	Message *ChannelMessage
}

// Fake 故意不按 ID 吞掉重复发送。超时可在记录触达后返回，暴露重试造成的重复。
type Fake struct {
	mu       sync.Mutex
	outcomes []error
	sent     []Sent
}

func (f *Fake) Outcomes(outcomes ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcomes = append(f.outcomes, outcomes...)
}

func (f *Fake) record(ctx context.Context, sent Sent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	var err error
	if len(f.outcomes) > 0 {
		err, f.outcomes = f.outcomes[0], f.outcomes[1:]
	}
	if err == nil || errors.Is(err, ErrUnknown) {
		f.sent = append(f.sent, sent)
	}
	if errors.Is(err, ErrUnknown) {
		return errors.Join(err, context.DeadlineExceeded)
	}
	return err
}

func (f *Fake) Push(ctx context.Context, owner task.Owner, message PushMessage) error {
	return f.record(ctx, Sent{Owner: owner, Push: &message})
}
func (f *Fake) Send(ctx context.Context, owner task.Owner, channel string, message ChannelMessage) error {
	return f.record(ctx, Sent{Owner: owner, Channel: channel, Message: &message})
}
func (f *Fake) Sent() []Sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]Sent(nil), f.sent...)
	for i := range out {
		if out[i].Push != nil {
			p := *out[i].Push
			out[i].Push = &p
		}
		if out[i].Message != nil {
			m := *out[i].Message
			out[i].Message = &m
		}
	}
	return out
}
