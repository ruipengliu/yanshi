package runtime

import (
	"context"
	"errors"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capability"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/outbox"
	"yanshi/internal/outbox/memoutbox"
)

// lostDeletionResult 保留真实写入，只丢掉一次 ToolResult 的成功响应。
// Worker 必须通过下一步同步识别已完成的调用，不能再次执行它。
type lostDeletionResult struct {
	*memlog.Log
	lose bool
	err  error
}

func (l *lostDeletionResult) Append(ctx context.Context, sid string, expected uint64, envelopes []outbox.Envelope, events ...*v1.Event) (uint64, error) {
	head, err := l.Log.Append(ctx, sid, expected, envelopes, events...)
	if err != nil {
		return 0, err
	}
	for _, e := range events {
		if l.lose && e.GetToolResult() != nil {
			l.lose = false
			return 0, l.err
		}
	}
	return head, nil
}

// 墓碑已写、日志尚在时，RunFailed 的信封复查也会得到 ErrGone。
// 通过真实 Step 的轮数上限进入 fail，覆盖成功提交与丢响应后同步两条路径。
func TestRunFailureAfterDeletionDropsWorker(t *testing.T) {
	for _, lost := range []bool{false, true} {
		name := "committed result"
		if lost {
			name = "lost result response"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			o := memoutbox.New()
			deletions := lifecycle.NewMemDeletionsWithOutbox(o)
			uncertain := errors.New("result committed but response lost")
			log := &lostDeletionResult{Log: memlog.NewWithOutbox(o), lose: lost, err: uncertain}
			invoked := 0
			probe := readOnlyProbe(true, &invoked)
			probe.S.Idempotent = false
			w, store, clk := readOnlyWorker(t, &capability.Catalog{Local: capability.NewRegistry(probe)}, "probe", log)
			store.Outbox, store.Deletions = o, deletions
			def, err := w.cfg.Agents.Get(w.st.Agent)
			if err != nil {
				t.Fatal(err)
			}
			def.MaxTurns = 1
			_, err = w.Step(ctx)
			if lost && !errors.Is(err, uncertain) || !lost && err != nil {
				t.Fatalf("result step: %v", err)
			}
			if invoked != 1 || lost && len(w.executed) != 1 {
				t.Fatalf("invoked=%d pending results=%d", invoked, len(w.executed))
			}
			if _, err := deletions.Mark(ctx, &lifecycle.Tombstone{SessionID: "s1", BusinessLine: "bl", Reason: "service", RequestedAt: clk.Now()}); err != nil {
				t.Fatal(err)
			}
			if did, err := w.Step(ctx); err != nil || !did {
				t.Fatalf("deleted session step: did=%v err=%v", did, err)
			}
			if w.lease != nil || w.st != nil || w.runID != "" || w.attempt != 0 || w.executed != nil || w.modelErrors != 0 || w.forceCompact || !w.failingSince.IsZero() {
				t.Fatal("deleted session retained worker state")
			}
			events, err := log.Read(ctx, "s1", 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			failures := 0
			for _, e := range events {
				if e.GetRunFailed() != nil {
					failures++
				}
			}
			if failures != 1 || invoked != 1 {
				t.Fatalf("failure path not exercised: failures=%d invoked=%d", failures, invoked)
			}
			rows, err := o.List(ctx, "bl")
			if err != nil || len(rows) != 1 || rows[0] != outbox.Deleted("bl", "s1", "service", "") {
				t.Fatalf("deletion envelopes=%+v err=%v", rows, err)
			}
		})
	}
}
