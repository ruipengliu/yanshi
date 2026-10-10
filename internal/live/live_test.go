package live

import (
	"strings"
	"sync"
	"testing"
)

func drain(ch <-chan Delta) []Delta {
	var out []Delta
	for {
		select {
		case d := <-ch:
			out = append(out, d)
		default:
			return out
		}
	}
}

// TestLateSubscriberGetsSnapshot：订阅时正在生成的消息先以一条 Snapshot 补发全文；
// 结束标记清除草稿；新的生成（不同的 AfterSeq）重新开始累积。
func TestLateSubscriberGetsSnapshot(t *testing.T) {
	b := NewMemBus()
	pub := func(after uint64, text string) {
		b.Publish(Delta{SessionID: "s", RunID: "r", Attempt: 1, AfterSeq: after, Text: text})
	}
	pub(5, "hello ")
	pub(5, "world")
	ch, cancel := b.Subscribe("s")
	defer cancel()
	got := drain(ch)
	if len(got) != 1 || !got[0].Snapshot || got[0].Text != "hello world" || got[0].AfterSeq != 5 {
		t.Fatalf("late subscriber got %+v", got)
	}

	b.Publish(Delta{SessionID: "s", RunID: "r", Attempt: 1, AfterSeq: 5, End: true})
	ch2, cancel2 := b.Subscribe("s")
	defer cancel2()
	if got := drain(ch2); len(got) != 0 {
		t.Fatalf("nothing should be replayed after the end marker, got %+v", got)
	}

	pub(7, "next")
	pub(8, "other") // 没有结束标记就换了位置（例如被接管）：重新开始
	ch3, cancel3 := b.Subscribe("s")
	defer cancel3()
	if got := drain(ch3); len(got) != 1 || got[0].Text != "other" || got[0].AfterSeq != 8 {
		t.Fatalf("snapshot after a new generation started = %+v", got)
	}
}

func TestOverflowedDraftIsNotReplayed(t *testing.T) {
	b := NewMemBus()
	b.Publish(Delta{SessionID: "s", AfterSeq: 1, Text: strings.Repeat("x", maxDraft)})
	b.Publish(Delta{SessionID: "s", AfterSeq: 1, Text: "y"})
	ch, cancel := b.Subscribe("s")
	defer cancel()
	if got := drain(ch); len(got) != 0 {
		t.Fatal("an incomplete draft must not be replayed as a snapshot")
	}
}

// TestSnapshotAndDeltasNeitherGapNorOverlap：发布与订阅并发时，Snapshot 加上其后的增量恰好是完整文本。
func TestSnapshotAndDeltasNeitherGapNorOverlap(t *testing.T) {
	for range 50 {
		b := NewMemBus()
		const n = 200
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range n {
				b.Publish(Delta{SessionID: "s", AfterSeq: 1, Text: string(rune('a' + i%26))})
			}
		}()
		ch, cancel := b.Subscribe("s")
		wg.Wait()
		var text strings.Builder
		for _, d := range drain(ch) {
			text.WriteString(d.Text)
		}
		cancel()
		var want strings.Builder
		for i := range n {
			want.WriteRune(rune('a' + i%26))
		}
		if text.String() != want.String() {
			t.Fatalf("draft = %q, want %q", text.String(), want.String())
		}
	}
}
