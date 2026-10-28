package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func holdSlot(t *testing.T, s *Slots, key string, limit int) func() {
	t.Helper()
	r, err := s.Acquire(context.Background(), key, limit, 0)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func waitSlotsQueued(t *testing.T, s *Slots, key string, n int) {
	t.Helper()
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		q := s.keys[key]
		return q != nil && len(q.waiting) == n
	})
}

func assertSlotsEmpty(t *testing.T, s *Slots) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.keys) != 0 {
		t.Fatalf("%d keys left", len(s.keys))
	}
}

func TestSlotsLimitFIFOAndKeyIsolation(t *testing.T) {
	var s Slots
	r1, r2 := holdSlot(t, &s, "a", 2), holdSlot(t, &s, "a", 2)
	other := holdSlot(t, &s, "b", 1)
	done := make(chan admissionResult, 3)
	for i := range 3 {
		go func() {
			r, err := s.Acquire(context.Background(), "a", 2, time.Minute)
			done <- admissionResult{i, r, err}
		}()
		waitSlotsQueued(t, &s, "a", i+1)
	}
	// 即时尝试也不能越过排队者。
	if r, err := s.Acquire(context.Background(), "a", 2, 0); r != nil || !errors.Is(err, ErrBusy) {
		t.Fatalf("immediate acquire: %v", err)
	}
	r1()
	for i := range 3 {
		nextAdmission(t, done, i)()
	}
	r2()
	other()
	assertSlotsEmpty(t, &s)
}

func TestSlotsTimeoutAndCancellation(t *testing.T) {
	for _, kind := range []string{"timeout", "cancel", "deadline", "granted"} {
		t.Run(kind, func(t *testing.T) {
			var s Slots
			held := holdSlot(t, &s, "key", 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wait := time.Minute
			want := error(context.Canceled)
			if kind == "timeout" {
				wait, want = 20*time.Millisecond, ErrBusy
			}
			if kind == "deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 20*time.Millisecond)
				defer stop()
				want = context.DeadlineExceeded
			}
			done := make(chan admissionResult, 1)
			go func() {
				r, err := s.Acquire(ctx, "key", 1, wait)
				done <- admissionResult{release: r, err: err}
			}()
			if kind == "cancel" || kind == "granted" {
				waitSlotsQueued(t, &s, "key", 1)
				if kind == "granted" {
					s.mu.Lock()
					cancel()
					s.releaseLocked("key", s.keys["key"])
					s.mu.Unlock()
				} else {
					cancel()
				}
			}
			got := <-done
			if !errors.Is(got.err, want) || got.release != nil {
				t.Fatalf("err %v, want %v", got.err, want)
			}
			if kind != "granted" {
				// 超时或取消不能扣掉仍被持有的名额。
				if _, err := s.Acquire(context.Background(), "key", 1, 0); !errors.Is(err, ErrBusy) {
					t.Fatalf("held slot was lost: %v", err)
				}
				held()
			}
			assertSlotsEmpty(t, &s)
		})
	}
}

func TestSlotsAcquireAllPartialFailure(t *testing.T) {
	reqs := []SlotReq{{Key: "total", Limit: 2}, {Key: "line", Limit: 1}, {Key: "name", Limit: 1}}
	for _, failed := range []int{1, 2} {
		var s Slots
		held := holdSlot(t, &s, reqs[failed].Key, 1)
		r, err := s.AcquireAll(context.Background(), reqs)
		if r != nil || !errors.Is(err, ErrBusy) {
			t.Fatalf("layer %d: err %v", failed, err)
		}
		s.mu.Lock()
		remaining := len(s.keys)
		s.mu.Unlock()
		if remaining != 1 {
			t.Fatalf("layer %d: %d keys left, want only blocker", failed, remaining)
		}
		held()
		assertSlotsEmpty(t, &s)
	}
}

func TestSlotsAcquireAllCancellationAtHandoff(t *testing.T) {
	var s Slots
	// 前两层已持有，第三层在交付时取消，三层都必须归还。
	_ = holdSlot(t, &s, "name", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan admissionResult, 1)
	go func() {
		r, err := s.AcquireAll(ctx, []SlotReq{
			{Key: "total", Limit: 2, Wait: time.Minute},
			{Key: "line", Limit: 1, Wait: time.Minute},
			{Key: "name", Limit: 1, Wait: time.Minute},
		})
		done <- admissionResult{release: r, err: err}
	}()
	waitSlotsQueued(t, &s, "name", 1)
	s.mu.Lock()
	cancel()
	s.releaseLocked("name", s.keys["name"])
	s.mu.Unlock()
	got := <-done
	if !errors.Is(got.err, context.Canceled) || got.release != nil {
		t.Fatalf("result %+v", got)
	}
	assertSlotsEmpty(t, &s)
}

func TestSlotsReleaseIdempotent(t *testing.T) {
	for _, all := range []bool{false, true} {
		var s Slots
		var release func()
		if all {
			var err error
			release, err = s.AcquireAll(context.Background(), []SlotReq{{Key: "total", Limit: 1}, {Key: "line", Limit: 1}})
			if err != nil {
				t.Fatal(err)
			}
		} else {
			release = holdSlot(t, &s, "total", 1)
		}
		var wg sync.WaitGroup
		for range 30 {
			wg.Add(1)
			go func() { defer wg.Done(); release() }()
		}
		wg.Wait()
		assertSlotsEmpty(t, &s)
		// 旧 release 不能归还同键新一轮取得的名额。
		fresh := holdSlot(t, &s, "total", 1)
		release()
		if _, err := s.Acquire(context.Background(), "total", 1, 0); !errors.Is(err, ErrBusy) {
			t.Fatalf("stale release freed fresh slot: %v", err)
		}
		fresh()
		assertSlotsEmpty(t, &s)
	}
}

func TestSlotsUnlimitedAndInvalidRequests(t *testing.T) {
	var s Slots
	var heldUnlimited []func()
	for _, limit := range []int{0, -1} {
		for range 100 {
			heldUnlimited = append(heldUnlimited, holdSlot(t, &s, "unlimited", limit))
		}
	}
	assertSlotsEmpty(t, &s)
	for _, release := range heldUnlimited {
		release()
		release()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, limit := range []int{0, 1} {
		if r, err := s.Acquire(ctx, "cancelled", limit, 0); r != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled acquire: %v", err)
		}
	}
	if _, err := s.AcquireAll(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled empty acquire: %v", err)
	}
	r, err := s.AcquireAll(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r()
	r()
	if r, err := s.AcquireAll(context.Background(), []SlotReq{{Key: "same", Limit: 1}, {Key: "same", Limit: 1}}); r != nil || err == nil {
		t.Fatal("duplicate keys accepted")
	}
	held := holdSlot(t, &s, "fixed", 1)
	if r, err := s.Acquire(context.Background(), "fixed", 2, 0); r != nil || err == nil || errors.Is(err, ErrBusy) {
		t.Fatalf("inconsistent limit: %v", err)
	}
	held()
	// 键空闲以后可以采用新配置。
	holdSlot(t, &s, "fixed", 2)()
	assertSlotsEmpty(t, &s)
}

func TestSlotsConcurrentCancellationStress(t *testing.T) {
	var s Slots
	const count = 200
	start, proceed := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	var cancels []context.CancelFunc
	for range count {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := s.Acquire(ctx, "shared", 4, time.Minute)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
				return
			}
			select {
			case <-proceed:
			case <-ctx.Done():
			}
			r()
			r()
		}()
	}
	close(start)
	waitSlotsQueued(t, &s, "shared", count-4)
	for i, cancel := range cancels {
		if i%2 == 0 {
			wg.Add(1)
			go func() { defer wg.Done(); cancel() }()
		}
	}
	close(proceed)
	wg.Wait()
	for _, cancel := range cancels {
		cancel()
	}
	assertSlotsEmpty(t, &s)
}
