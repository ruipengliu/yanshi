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
	"yanshi/internal/workqueue/memqueue"
)

// updateFixture 是一个有活跃 Run（r1，Attempt 1）的 Session，以及读写它的 Store 与已加载的投影。
func updateFixture(t *testing.T, log eventlog.Log) (*Store, *State) {
	t.Helper()
	ctx := context.Background()
	if _, err := log.Append(ctx, "s", 0, build(created(), requested("r1"), attempt("r1", 1))...); err != nil {
		t.Fatal(err)
	}
	store := &Store{Log: log, IDs: ids.Sequential("e"), Clock: clock.Real{}}
	st, err := store.Load(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	return store, st
}

func head(t *testing.T, log eventlog.Log) uint64 {
	t.Helper()
	events, err := eventlog.ReadAll(context.Background(), log, "s", 0)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(len(events))
}

// 冲突后以同步后的投影重新决策，而不是重放原来的事件。
func TestUpdateRedecidesAfterConflict(t *testing.T) {
	ctx := context.Background()
	log := memlog.New()
	store, st := updateFixture(t, log)
	var seen []uint64
	err := store.Update(ctx, st, func(st *State) (Decision, error) {
		seen = append(seen, st.Seq)
		if len(seen) == 1 {
			// 决策与提交之间，另一个写入方追加了事件。
			other, err := store.Load(ctx, "s")
			if err != nil {
				return Decision{}, err
			}
			if err := store.Commit(ctx, other, steered("r1")); err != nil {
				return Decision{}, err
			}
		}
		return Decision{Events: []*v1.Event{assistant("r1", 1)}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != 3 || seen[1] != 4 {
		t.Fatalf("decide saw seqs %v, want [3 4]", seen)
	}
	if st.Seq != 5 || head(t, log) != 5 {
		t.Fatalf("state seq = %d, log head = %d, want 5", st.Seq, head(t, log))
	}
}

func TestUpdateStopsOnDecideError(t *testing.T) {
	log := memlog.New()
	store, st := updateFixture(t, log)
	boom := errors.New("boom")
	calls := 0
	err := store.Update(context.Background(), st, func(*State) (Decision, error) {
		calls++
		return Decision{Events: []*v1.Event{steered("r1")}}, boom
	})
	if err != boom {
		t.Fatalf("err = %v, want the decide error itself", err)
	}
	if calls != 1 || head(t, log) != 3 || st.Seq != 3 {
		t.Fatalf("calls = %d, log head = %d, seq = %d; want 1 call and nothing written", calls, head(t, log), st.Seq)
	}
}

func TestUpdateZeroEventsWritesNothing(t *testing.T) {
	log := memlog.New()
	store, st := updateFixture(t, log)
	calls := 0
	err := store.Update(context.Background(), st, func(*State) (Decision, error) {
		calls++
		return Decision{Wake: true}, nil // Wake 不带事件时同样不写、不入队
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || head(t, log) != 3 || st.Seq != 3 {
		t.Fatalf("calls = %d, log head = %d, seq = %d; want 1 call and nothing written", calls, head(t, log), st.Seq)
	}
}

// 校验错误不重试：同样的写入再试也不会成功。
func TestUpdateReturnsInvalid(t *testing.T) {
	log := memlog.New()
	store, st := updateFixture(t, log)
	calls := 0
	err := store.Update(context.Background(), st, func(*State) (Decision, error) {
		calls++
		return Decision{Events: []*v1.Event{completed("r2", 1)}}, nil
	})
	if !errors.Is(err, ErrInvalid) || calls != 1 || head(t, log) != 3 {
		t.Fatalf("err = %v, calls = %d, log head = %d; want ErrInvalid after 1 call, nothing written", err, calls, head(t, log))
	}
}

func TestUpdateWake(t *testing.T) {
	ctx := context.Background()
	ready := func(q *memqueue.Queue) int {
		depths, err := q.Depths(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, d := range depths {
			n += d.Ready
		}
		return n
	}
	t.Run("wake", func(t *testing.T) {
		log := memlog.New()
		q := memqueue.New(clock.Real{})
		store, st := updateFixture(t, log)
		store.Waking = log.Waking(q)
		if err := store.Update(ctx, st, func(*State) (Decision, error) {
			return Decision{Events: []*v1.Event{steered("r1")}, Wake: true}, nil
		}); err != nil {
			t.Fatal(err)
		}
		if head(t, log) != 4 || ready(q) != 1 {
			t.Fatalf("log head = %d, ready = %d; want 4 and 1", head(t, log), ready(q))
		}
	})
	t.Run("no wake", func(t *testing.T) {
		log := memlog.New()
		q := memqueue.New(clock.Real{})
		store, st := updateFixture(t, log)
		store.Waking = log.Waking(q)
		if err := store.Update(ctx, st, func(*State) (Decision, error) {
			return Decision{Events: []*v1.Event{steered("r1")}}, nil
		}); err != nil {
			t.Fatal(err)
		}
		if head(t, log) != 4 || ready(q) != 0 {
			t.Fatalf("log head = %d, ready = %d; want 4 and 0", head(t, log), ready(q))
		}
	})
	t.Run("no waking log", func(t *testing.T) {
		log := memlog.New()
		store, st := updateFixture(t, log)
		err := store.Update(ctx, st, func(*State) (Decision, error) {
			return Decision{Events: []*v1.Event{steered("r1")}, Wake: true}, nil
		})
		if !errors.Is(err, ErrNoWaking) || head(t, log) != 3 {
			t.Fatalf("err = %v, log head = %d; want ErrNoWaking and nothing written", err, head(t, log))
		}
	})
}

// 日志在决策与提交之间被删除：冲突后同步读不到新事件，返回 ErrGone，不再重试。
func TestUpdateGoneAfterDeletion(t *testing.T) {
	ctx := context.Background()
	log := memlog.New()
	store, st := updateFixture(t, log)
	calls := 0
	err := store.Update(ctx, st, func(*State) (Decision, error) {
		calls++
		if err := log.Delete(ctx, "s"); err != nil {
			return Decision{}, err
		}
		return Decision{Events: []*v1.Event{steered("r1")}}, nil
	})
	if !errors.Is(err, ErrGone) || calls != 1 || head(t, log) != 0 {
		t.Fatalf("err = %v, calls = %d, log head = %d; want ErrGone after 1 call, empty log", err, calls, head(t, log))
	}
}

// alwaysConflicting 每次追加都先让日志前进一条（别的写入方抢先），再报告冲突。
type alwaysConflicting struct{ *memlog.Log }

func (l alwaysConflicting) Append(ctx context.Context, sid string, expected uint64, _ ...*v1.Event) (uint64, error) {
	if _, err := l.Log.Append(ctx, sid, expected, steered("r1")); err != nil {
		return 0, err
	}
	return 0, eventlog.ErrConflict
}

func TestUpdateTooManyConflicts(t *testing.T) {
	ctx := context.Background()
	inner := memlog.New()
	store, st := updateFixture(t, inner)
	store.Log = alwaysConflicting{inner}
	calls := 0
	err := store.Update(ctx, st, func(*State) (Decision, error) {
		calls++
		return Decision{Events: []*v1.Event{assistant("r1", 1)}}, nil
	})
	if !errors.Is(err, ErrTooManyConflicts) {
		t.Fatalf("err = %v, want ErrTooManyConflicts", err)
	}
	if calls != MaxCommitRetries {
		t.Fatalf("decide called %d times, want %d", calls, MaxCommitRetries)
	}
	// 每次冲突后都同步到了别人追加的事件。
	if want := uint64(3 + MaxCommitRetries); st.Seq != want {
		t.Fatalf("seq = %d, want %d", st.Seq, want)
	}
}
