package feed

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/live"
	"yanshi/internal/session"
)

// flakyWait 的前 n 次 Wait 立即失败（数据库短暂不可用）。
type flakyWait struct {
	*memlog.Log
	mu sync.Mutex
	n  int
}

func (l *flakyWait) Wait(ctx context.Context, sid string, after uint64) (uint64, error) {
	l.mu.Lock()
	fail := l.n > 0
	l.n--
	l.mu.Unlock()
	if fail {
		return 0, errors.New("database unavailable")
	}
	return l.Log.Wait(ctx, sid, after)
}

type chanSink struct{ events chan *v1.Event }

func (s chanSink) Event(e *v1.Event) error { s.events <- e; return nil }
func (chanSink) Delta(live.Delta) error    { return nil }
func (chanSink) Ping() error               { return nil }
func (chanSink) Flush() error              { return nil }

// 等待日志前进的出错是暂时的：恢复后流继续推送新事件，而不是停在那里只保活。
func TestStreamRecoversFromWaitErrors(t *testing.T) {
	waitRetryMin, waitRetryMax = time.Millisecond, 5*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := &flakyWait{Log: memlog.New(), n: 3}
	ev := func(i int) *v1.Event {
		return &v1.Event{Payload: &v1.Event_RunRequested{RunRequested: &v1.RunRequested{RunId: "r" + string(rune('0'+i))}}}
	}
	if _, err := log.Append(ctx, "s1", 0, ev(1)); err != nil {
		t.Fatal(err)
	}
	sink := chanSink{events: make(chan *v1.Event, 8)}
	stopped := make(chan struct{})
	go func() {
		_ = Source{Log: log, Live: live.Discard{}}.Stream(ctx, &session.State{SessionID: "s1"}, 0, sink)
		close(stopped)
	}()
	defer func() { cancel(); <-stopped }() // 先于下一个用例修改退避参数结束
	for i := 1; i <= 3; i++ {
		if i > 1 {
			if _, err := log.Append(ctx, "s1", uint64(i-1), ev(i)); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case e := <-sink.events:
			if e.GetSeq() != uint64(i) {
				t.Fatalf("got seq %d, want %d", e.GetSeq(), i)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("event %d never delivered", i)
		}
	}
}

// TestStreamEndsWhenSessionDeleted：删除记录写入后流立即结束（ErrDeleted），不等保活周期的复查。
func TestStreamEndsWhenSessionDeleted(t *testing.T) {
	ctx := context.Background()
	dels := lifecycle.NewMemDeletions()
	done := make(chan error, 1)
	go func() {
		src := Source{Log: memlog.New(), Live: live.Discard{}, Deletions: dels, PingInterval: time.Hour}
		done <- src.Stream(ctx, &session.State{SessionID: "s1"}, 0, chanSink{events: make(chan *v1.Event, 8)})
	}()
	time.Sleep(20 * time.Millisecond)
	if _, err := dels.Mark(ctx, &lifecycle.Tombstone{SessionID: "s1", BusinessLine: "bl", Reason: "user", RequestedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrDeleted) {
			t.Fatalf("stream ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream still open after the session was deleted")
	}
}
