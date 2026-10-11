package session

import (
	"context"
	"errors"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
)

// stampingLog 在判定冲突之前就给事件编号（早先 pglog 的行为）。
type stampingLog struct{ *memlog.Log }

func (l stampingLog) Append(ctx context.Context, sid string, expected uint64, events ...*v1.Event) (uint64, error) {
	for i, e := range events {
		e.Seq = expected + uint64(i) + 1
	}
	return l.Log.Append(ctx, sid, expected, events...)
}

// 冲突后同步、以同一批事件重试必须成功：Worker 与 Hub 的冲突重试都复用原来的事件。
func TestCommitRetriesSameEventsAfterConflict(t *testing.T) {
	ctx := context.Background()
	for name, log := range map[string]eventlog.Log{"memlog": memlog.New(), "stamping": stampingLog{memlog.New()}} {
		t.Run(name, func(t *testing.T) {
			store := &Store{Log: log, IDs: ids.Sequential("e"), Clock: clock.Real{}}
			if _, err := log.Append(ctx, "s", 0, build(created(), requested("r1"), attempt("r1", 1))...); err != nil {
				t.Fatal(err)
			}
			mine, err := store.Load(ctx, "s")
			if err != nil {
				t.Fatal(err)
			}
			other, _ := store.Load(ctx, "s")
			if err := store.Commit(ctx, other, steered("r1")); err != nil {
				t.Fatal(err)
			}
			msg := assistant("r1", 1)
			if err := store.Commit(ctx, mine, msg); !errors.Is(err, eventlog.ErrConflict) {
				t.Fatalf("err = %v, want conflict", err)
			}
			if err := store.SyncAfterConflict(ctx, mine); err != nil {
				t.Fatal(err)
			}
			if err := store.Commit(ctx, mine, msg); err != nil {
				t.Fatalf("retry after conflict: %v", err)
			}
			if mine.Seq != 5 {
				t.Fatalf("seq = %d, want 5", mine.Seq)
			}
		})
	}
}
