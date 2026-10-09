package workqueue

import (
	"context"
	"sync"
	"time"
)

// IdleGate 让同一进程中同一时刻至多一个空闲消费者（Worker、沙箱控制器）轮询工作队列
// （docs/design/m2-scale-test.md §5）。
//
// 不加门控时，每个空闲 Worker 各自每 IdleWait 认领一次，空闲开销与 Worker 总数成正比。
// 加门控后，持有门控的 Worker 在入队信号到达时立即认领，否则指数退避轮询（兜底挂起到期与租约过期）；
// 认领到工作后离开门控，由下一个空闲消费者接替。门控只影响循环的节奏，不进入 Step，
// 因此不影响执行语义与模拟测试。
type IdleGate struct {
	// Ready 是入队信号（Signaler），可为 nil。
	Ready <-chan struct{}
	// Min、Max 是没有信号时的轮询退避区间，默认 50ms 与 1s。
	Min, Max time.Duration

	once sync.Once
	sem  chan struct{}
}

func (g *IdleGate) init() {
	g.once.Do(func() {
		g.sem = make(chan struct{}, 1)
		if g.Min == 0 {
			g.Min = 50 * time.Millisecond
		}
		if g.Max == 0 {
			g.Max = time.Second
		}
	})
}

// Idle 取得门控后反复调用 try，直到 try 报告应当离开门控（认领到工作）或 ctx 结束。
func (g *IdleGate) Idle(ctx context.Context, try func() bool) {
	g.init()
	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-g.sem }()
	delay := g.Min
	for ctx.Err() == nil {
		if try() {
			return
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
		case <-g.Ready:
			delay = g.Min
		case <-t.C:
			delay = min(delay*2, g.Max)
		}
		t.Stop()
	}
}
