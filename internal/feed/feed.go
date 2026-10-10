// Package feed 是一个 Session 对外的事件流：从日志续传的已提交事件，加上实时增量。
// HTTP 的 SSE 与 Connection 上的订阅（docs/design/m3-duplex-channel.md）共用它，二者语义一致。
package feed

import (
	"context"
	"errors"
	"time"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/live"
	"yanshi/internal/session"
)

// ErrDeleted 表示 Session 在订阅期间被删除（ADR-0015）。
var ErrDeleted = errors.New("feed: session deleted")

// Sink 接收事件流。任一方法返回错误即结束 Stream。
type Sink interface {
	Event(e *v1.Event) error
	Delta(d live.Delta) error
	// Ping 在空闲时周期性调用，传输层借此保活。
	Ping() error
	// Flush 在一批消息之后调用。
	Flush() error
}

type Source struct {
	Log  eventlog.Log
	Live live.Bus
	// Deletions 非 nil 时借保活周期复查删除记录：删除后日志不再增长，流需要主动结束。
	Deletions lifecycle.Deletions
	// PingInterval 默认 15 秒。
	PingInterval time.Duration
}

// Stream 推送 seq 大于 after 的已提交事件，之后持续推送新事件与增量，直到 ctx 结束、
// Sink 返回错误或 Session 被删除（ErrDeleted）。st 是调用方已读到的投影，用来确定当前的执行进程。
func (s Source) Stream(ctx context.Context, st *session.State, after uint64, sink Sink) error {
	id := st.SessionID
	// 先订阅增量再读日志，避免两者之间的增量丢失。增量总线支持跟随时，由本流告知当前执行进程
	// （投影中活跃 Run 的当前 Attempt，之后按读到的事件推进），总线不必再读一遍日志。
	var deltas <-chan live.Delta
	follow := func([]*v1.Event) {}
	if f, ok := s.Live.(live.Follower); ok {
		ch, setEndpoint, cancel := f.SubscribeFollowing(id)
		defer cancel()
		endpoint := ""
		if a := st.Active(); a != nil && a.Status == session.RunRunning {
			endpoint = a.LiveEndpoint
		}
		setEndpoint(endpoint)
		// 已读到 st.Seq 的投影；之后只按新读到的事件推进。
		seen := st.Seq
		deltas, follow = ch, func(events []*v1.Event) {
			var fresh []*v1.Event
			for _, e := range events {
				if e.GetSeq() > seen {
					fresh = append(fresh, e)
					seen = e.GetSeq()
				}
			}
			if len(fresh) > 0 {
				endpoint = live.Endpoint(endpoint, fresh)
				setEndpoint(endpoint)
			}
		}
	} else {
		ch, unsubscribe := s.Live.Subscribe(id)
		defer unsubscribe()
		deltas = ch
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	heads := make(chan struct{}, 1)
	go waitHeads(ctx, s.Log, id, after, heads)
	interval := s.PingInterval
	if interval == 0 {
		interval = 15 * time.Second
	}
	ping := time.NewTicker(interval)
	defer ping.Stop()

	// 只在日志前进时读日志：增量逐 token 到达，不应每个都引起一次读取。
	read := true
	for {
		if read {
			events, err := eventlog.ReadAll(ctx, s.Log, id, after)
			if err != nil {
				return err
			}
			for _, e := range events {
				if err := sink.Event(Public(e)); err != nil {
					return err
				}
				after = e.GetSeq()
			}
			follow(events)
			read = false
		}
		if err := sink.Flush(); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ping.C:
			if s.Deletions != nil {
				if gone, _ := lifecycle.Deleted(ctx, s.Deletions, id); gone {
					return ErrDeleted
				}
			}
			if err := sink.Ping(); err != nil {
				return err
			}
		case <-heads:
			read = true
		case d := <-deltas:
			if err := sink.Delta(d); err != nil {
				return err
			}
		}
	}
}

// Public 返回可以发给客户端的事件：清除进程内部地址（ADR-0013）。事件不可修改，因此按需复制。
func Public(e *v1.Event) *v1.Event {
	a := e.GetAttemptStarted()
	if a.GetLiveEndpoint() == "" {
		return e
	}
	cp := proto.Clone(e).(*v1.Event)
	cp.GetAttemptStarted().LiveEndpoint = ""
	return cp
}

// waitHeads 每当日志前进时向 heads 发一个合并后的通知。
func waitHeads(ctx context.Context, log eventlog.Log, id string, after uint64, heads chan<- struct{}) {
	for {
		head, err := log.Wait(ctx, id, after)
		if err != nil {
			return
		}
		after = head
		select {
		case heads <- struct{}{}:
		default:
		}
	}
}
