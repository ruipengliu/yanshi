package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"yanshi/internal/clock"
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

// Retry-After 不受 MaxBackoff 截断：提前重试只会再被限流一次。
func TestGuardHonorsLongRetryAfter(t *testing.T) {
	p := &scripted{errs: []error{&ProviderError{Status: 429, RetryAfter: time.Minute}}}
	g, waits := newGuard(p, Limits{Retries: 1, MaxBackoff: 30 * time.Second})
	if _, err := g.Generate(context.Background(), &Request{Model: "m"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(*waits) != 1 || (*waits)[0] != time.Minute {
		t.Fatalf("waits %v, want [1m]", *waits)
	}
}

// 总时限内等不到下一次重试时立即放弃，交还提供商的错误，而不是睡到时限到期。
func TestGuardGivesUpWhenRetryWouldMissDeadline(t *testing.T) {
	p := &scripted{errs: []error{&ProviderError{Status: 429, RetryAfter: time.Minute}}}
	g, waits := newGuard(p, Limits{Retries: 2, Deadline: time.Second})
	_, err := g.Generate(context.Background(), &Request{Model: "m"}, nil)
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Status != 429 || len(*waits) != 0 || p.calls != 1 {
		t.Fatalf("err %v, waits %v, calls %d", err, *waits, p.calls)
	}
}

// 在并发上限处排队计入总时限：上游被占满时，调用在时限到期时返回 ErrTimeout，而不是无限排队。
func TestGuardDeadlineCoversQueueing(t *testing.T) {
	release, started := make(chan struct{}), make(chan struct{}, 1)
	// 占着名额的调用不理会自己的时限，直到测试放行：否则它到期释放名额时，排队的调用可能恰好在自己到期前拿到名额
	// 而成功（机器繁忙时出现过）。
	busy := providerFunc(func(context.Context, *Request, func(Delta)) (*Response, error) {
		started <- struct{}{}
		<-release
		return &Response{}, nil
	})
	g := &Guard{Name: "p", Provider: busy, Limits: Limits{MaxConcurrency: 1, Deadline: 50 * time.Millisecond}}
	held := make(chan error, 1)
	go func() {
		_, err := g.Generate(context.Background(), &Request{Model: "m"}, nil)
		held <- err
	}()
	<-started
	start := time.Now()
	_, err := g.Generate(context.Background(), &Request{Model: "m"}, nil)
	if !errors.Is(err, ErrTimeout) || time.Since(start) > time.Second {
		t.Fatalf("queued call: err %v after %s", err, time.Since(start))
	}
	close(release)
	if err := <-held; err != nil {
		t.Fatal(err)
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

func TestGuardLimitsConcurrency(t *testing.T) {
	var mu sync.Mutex
	inflight, peak := 0, 0
	proceed, started := make(chan struct{}), make(chan struct{}, 30)
	p := providerFunc(func(ctx context.Context, _ *Request, _ func(Delta)) (*Response, error) {
		mu.Lock()
		inflight++
		peak = max(peak, inflight)
		mu.Unlock()
		started <- struct{}{}
		<-proceed
		mu.Lock()
		inflight--
		mu.Unlock()
		return &Response{}, nil
	})
	g := &Guard{Name: "p", Provider: p, Limits: Limits{MaxConcurrency: 3}}
	g.init()
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := g.Generate(classContext(admissionClass(i%4)), &Request{}, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	for range 3 {
		<-started
	}
	waitQueued(t, g.gate, 27)
	for range 30 {
		proceed <- struct{}{}
	}
	wg.Wait()
	if peak != 3 {
		t.Fatalf("peak concurrency %d, want 3", peak)
	}
	assertAdmissionEmpty(t, g.gate)
}

func TestGuardQueuesByClassBeforeWaitingForRate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	clk := clock.NewFake(time.Unix(0, 0))
	waitingForToken := make(chan admissionClass, 2)
	called := make(chan admissionClass, 2)
	g := &Guard{
		Name:   "rate-after-admission",
		Limits: Limits{MaxConcurrency: 1, RPS: 1},
		Provider: providerFunc(func(ctx context.Context, _ *Request, _ func(Delta)) (*Response, error) {
			called <- classOf(ctx)
			return &Response{}, nil
		}),
		sleep: func(ctx context.Context, d time.Duration) error {
			waitingForToken <- classOf(ctx)
			clk.Advance(d)
			return ctx.Err()
		},
	}
	g.init()
	g.rate.now = clk.Now
	// 模拟一个已经用掉令牌、仍占着唯一并发名额的调用。
	held := holdAdmission(t, g.gate, interactive)
	defer held()
	if err := g.rate.wait(ctx, g.sleep); err != nil {
		t.Fatal(err)
	}
	for i, callCtx := range []context.Context{WithBackground(ctx), ctx} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := g.Generate(callCtx, &Request{}, nil); err != nil && ctx.Err() == nil {
				t.Error(err)
			}
		}()
		waitQueued(t, g.gate, i+1)
		if len(waitingForToken) > 0 {
			t.Fatal("a queued call reached the rate limiter before admission")
		}
	}
	held()
	wg.Wait()
	if len(waitingForToken) != 2 || len(called) != 2 {
		t.Fatalf("got %d token waits and %d calls, want two of each", len(waitingForToken), len(called))
	}
	for _, want := range []admissionClass{interactive, backgroundRun} {
		if waiting, started := <-waitingForToken, <-called; waiting != want || started != want {
			t.Fatalf("token wait from %v, call from %v, want both from %v", waiting, started, want)
		}
	}
	assertAdmissionEmpty(t, g.gate)
}

func TestTokenBucketPacesCalls(t *testing.T) {
	b := &tokenBucket{rate: 100, burst: 5, now: time.Now}
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
