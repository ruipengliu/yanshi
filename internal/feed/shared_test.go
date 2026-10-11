package feed

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/live"
	"yanshi/internal/session"
)

// countingLog 统计读取日志的次数。
type countingLog struct {
	*memlog.Log
	reads atomic.Int32
}

func (l *countingLog) Read(ctx context.Context, sid string, after uint64, limit int) ([]*v1.Event, error) {
	l.reads.Add(1)
	return l.Log.Read(ctx, sid, after, limit)
}

// slowSink 在 gate 关闭之前阻塞每一次推送：一个跟不上的订阅者。
type slowSink struct {
	chanSink
	gate chan struct{}
}

func (s slowSink) Event(e *v1.Event) error { <-s.gate; return s.chanSink.Event(e) }

func appendN(t *testing.T, l *memlog.Log, from, n int) {
	t.Helper()
	for i := range n {
		e := &v1.Event{Payload: &v1.Event_Steered{Steered: &v1.Steered{RunId: fmt.Sprint(from + i)}}}
		if _, err := l.Append(context.Background(), "s1", uint64(from+i-1), e); err != nil {
			t.Fatal(err)
		}
	}
}

func receive(t *testing.T, sink chanSink, from, to uint64) {
	t.Helper()
	for want := from; want <= to; want++ {
		select {
		case e := <-sink.events:
			if e.GetSeq() != want {
				t.Fatalf("got seq %d, want %d", e.GetSeq(), want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("seq %d never delivered", want)
		}
	}
}

// 同一 Session 的订阅者共用一个读者：新事件只读一次日志，与订阅数无关；跟不上的订阅者从日志补读，不丢事件、
// 不拖慢其他订阅者；最后一个订阅者离开后读者结束。
func TestHubSharesOneReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	log := &countingLog{Log: memlog.New()}
	appendN(t, log.Log, 1, 3)
	hub := &Hub{Log: log}
	src := Source{Log: log, Live: live.Discard{}, Hub: hub}

	const subscribers = 20
	sinks := make([]chanSink, subscribers)
	var wg sync.WaitGroup
	for i := range sinks {
		sinks[i] = chanSink{events: make(chan *v1.Event, 1024)}
		wg.Go(func() { _ = src.Stream(ctx, &session.State{SessionID: "s1"}, 0, sinks[i]) })
	}
	slow := slowSink{chanSink: chanSink{events: make(chan *v1.Event, 1024)}, gate: make(chan struct{})}
	wg.Go(func() { _ = src.Stream(ctx, &session.State{SessionID: "s1"}, 0, slow) })
	for _, s := range sinks {
		receive(t, s, 1, 3) // 各自补齐此前的事件
	}

	before := log.reads.Load()
	const fresh = 100
	for i := range fresh {
		appendN(t, log.Log, 4+i, 1)
		for _, s := range sinks {
			receive(t, s, uint64(4+i), uint64(4+i))
		}
	}
	// 读者每次日志前进读一次（可能合并），加上慢订阅者跟不上时的补读；与 20 个订阅者无关。
	if reads := log.reads.Load() - before; reads > fresh+5 {
		t.Fatalf("%d log reads for %d events and %d subscribers", reads, fresh, subscribers+1)
	}
	close(slow.gate)
	receive(t, slow.chanSink, 1, 3+fresh)

	cancel()
	wg.Wait()
	if n := hub.readers(); n != 0 {
		t.Fatalf("%d readers left after every subscriber left", n)
	}
}
