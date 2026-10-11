package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// scripted 依次返回 errs 中的错误（nil 表示成功）；emit 为真时失败前先输出一段增量。
type scripted struct {
	mu    sync.Mutex
	errs  []error
	emit  bool
	calls int
}

func (p *scripted) Generate(ctx context.Context, _ *Request, onDelta func(Delta)) (*Response, error) {
	p.mu.Lock()
	i := p.calls
	p.calls++
	p.mu.Unlock()
	var err error
	if i < len(p.errs) {
		err = p.errs[i]
	}
	if err != nil && p.emit && onDelta != nil {
		onDelta(Delta{Text: "部分"})
	}
	if err != nil {
		return nil, err
	}
	return &Response{Content: TextBlocks("好")}, nil
}

func newGuard(p Provider, l Limits) (*Guard, *[]time.Duration) {
	var waits []time.Duration
	g := &Guard{Name: "p", Provider: p, Limits: l}
	g.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	return g, &waits
}

func TestGuardRetriesTransientFailures(t *testing.T) {
	p := &scripted{errs: []error{&ProviderError{Status: 429, RetryAfter: 5 * time.Second}, &ProviderError{Status: 503}}}
	g, waits := newGuard(p, Limits{Retries: 2, Backoff: 100 * time.Millisecond, MaxBackoff: 10 * time.Second})
	resp, err := g.Generate(context.Background(), &Request{Model: "m"}, func(Delta) {})
	if err != nil || Text(resp.Content) != "好" || p.calls != 3 {
		t.Fatalf("resp %v, err %v, calls %d", resp, err, p.calls)
	}
	// 第一次等待取提供商的 Retry-After；第二次是带抖动的指数退避 [100ms, 200ms]。
	if (*waits)[0] != 5*time.Second || (*waits)[1] < 100*time.Millisecond || (*waits)[1] > 200*time.Millisecond {
		t.Fatalf("waits %v", *waits)
	}
}

func TestGuardGivesUp(t *testing.T) {
	for name, tc := range map[string]struct {
		errs  []error
		emit  bool
		calls int
	}{
		"retries exhausted":       {errs: []error{&ProviderError{Status: 500}, &ProviderError{Status: 500}, &ProviderError{Status: 500}}, calls: 3},
		"output already streamed": {errs: []error{&ProviderError{Status: 0}}, emit: true, calls: 1},
		"bad request":             {errs: []error{&ProviderError{Status: 400}}, calls: 1},
		"context overflow":        {errs: []error{ErrContextOverflow}, calls: 1},
	} {
		t.Run(name, func(t *testing.T) {
			p := &scripted{errs: tc.errs, emit: tc.emit}
			g, _ := newGuard(p, Limits{Retries: 2})
			if _, err := g.Generate(context.Background(), &Request{Model: "m"}, func(Delta) {}); err == nil || p.calls != tc.calls {
				t.Fatalf("err %v, calls %d, want %d", err, p.calls, tc.calls)
			}
		})
	}
}

type providerFunc func(context.Context, *Request, func(Delta)) (*Response, error)

func (f providerFunc) Generate(ctx context.Context, req *Request, onDelta func(Delta)) (*Response, error) {
	return f(ctx, req, onDelta)
}

// hanging 阻塞到 ctx 结束：上游卡住。
type hanging struct{}

func (hanging) Generate(ctx context.Context, _ *Request, _ func(Delta)) (*Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestGuardTimesOut(t *testing.T) {
	g, waits := newGuard(hanging{}, Limits{Timeout: 20 * time.Millisecond, Retries: 3})
	_, err := g.Generate(context.Background(), &Request{Model: "m"}, nil)
	if !errors.Is(err, ErrTimeout) || len(*waits) != 0 {
		t.Fatalf("err %v, retried %d times; a timeout should not be retried", err, len(*waits))
	}
}

func TestRetryable(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&ProviderError{Status: 429}, true},
		{&ProviderError{Status: 502}, true},
		{&ProviderError{Status: 0}, true},
		{&ProviderError{Status: 401}, false},
		{ErrStalled, true},
		{ErrTimeout, false},
		{context.Canceled, false},
		{errors.Join(ErrContextOverflow, &ProviderError{Status: 400}), false},
		{errors.New("decode failed"), false},
	} {
		if got := Retryable(tc.err); got != tc.want {
			t.Errorf("Retryable(%v) = %v", tc.err, got)
		}
	}
}

// TestAdmissionPrefersInteractive：名额满时排队；名额释放后先给交互调用，再给长任务的调用。
func TestAdmissionPrefersInteractive(t *testing.T) {
	a := &admission{limit: 1}
	ctx := context.Background()
	if err := a.acquire(ctx, false); err != nil {
		t.Fatal(err)
	}
	order := make(chan string, 2)
	var wg sync.WaitGroup
	wait := func(name string, bg bool) {
		defer wg.Done()
		if err := a.acquire(ctx, bg); err != nil {
			t.Error(err)
			return
		}
		order <- name
		a.release()
	}
	wg.Add(2)
	go wait("background", true)
	waitQueued(t, a, 1)
	go wait("interactive", false)
	waitQueued(t, a, 2)
	a.release()
	wg.Wait()
	if first := <-order; first != "interactive" {
		t.Fatalf("%s got the slot first", first)
	}
	if a.inflight != 0 {
		t.Fatalf("inflight %d after all released", a.inflight)
	}
}

func TestAdmissionCancelledWaiterDoesNotLeak(t *testing.T) {
	a := &admission{limit: 1}
	_ = a.acquire(context.Background(), false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- a.acquire(ctx, false) }()
	waitQueued(t, a, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	a.release()
	if a.inflight != 0 || len(a.waiting[0]) != 0 {
		t.Fatalf("inflight %d, waiting %d", a.inflight, len(a.waiting[0]))
	}
}

func waitQueued(t *testing.T, a *admission, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		a.mu.Lock()
		got := len(a.waiting[0]) + len(a.waiting[1])
		a.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d waiting, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGuardLimitsConcurrency(t *testing.T) {
	var mu sync.Mutex
	inflight, peak := 0, 0
	p := providerFunc(func(ctx context.Context, _ *Request, _ func(Delta)) (*Response, error) {
		mu.Lock()
		inflight++
		peak = max(peak, inflight)
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		mu.Lock()
		inflight--
		mu.Unlock()
		return &Response{}, nil
	})
	g := &Guard{Name: "p", Provider: p, Limits: Limits{MaxConcurrency: 3, RPS: 2000}}
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			if i%2 == 0 {
				ctx = WithBackground(ctx)
			}
			if _, err := g.Generate(ctx, &Request{}, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if peak > 3 || peak == 0 {
		t.Fatalf("peak concurrency %d, limit 3", peak)
	}
}

func TestTokenBucketPacesCalls(t *testing.T) {
	b := &tokenBucket{rate: 100, burst: 5}
	start := time.Now()
	for range 15 {
		if err := b.wait(context.Background(), sleepCtx); err != nil {
			t.Fatal(err)
		}
	}
	// 突发 5 个之后，其余 10 个按每秒 100 个发出：至少约 100ms。
	if d := time.Since(start); d < 80*time.Millisecond {
		t.Fatalf("15 calls took %s; the bucket did not pace them", d)
	}
}
