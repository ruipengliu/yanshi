package workqueue

import (
	"context"
	"testing"
	"time"
)

func TestFanoutWakesEveryReceiver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{}, 1)
	outs := Fanout(ctx, ready, 2)
	for range 2 { // 第二轮：接收方取走信号后，下一个信号同样送达
		ready <- struct{}{}
		for i, o := range outs {
			select {
			case <-o:
			case <-time.After(time.Second):
				t.Fatalf("receiver %d not woken", i)
			}
		}
	}
}
