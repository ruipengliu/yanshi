package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"slices"
	"sync"
	"syscall"
	"time"

	"yanshi/internal/metrics"
)

// 调用提供商的约束（延展性评审 §4.2）：每次调用有时限；可重试的失败（限流、服务端错误、连接中断）以带抖动的
// 指数退避重试；每个提供商一个准入器，限制并发与发起速率，排队时交互调用优先（ADR-0029）。
// 没有这些，上游卡住时 Worker 永远持有租约，Run 停在 running；配额触顶时所有 Worker 一齐重试，加剧拥塞。

// ErrTimeout 表示一次调用超过了时限。它不重试：同样的请求多半同样耗时，交给 Worker 的模型错误计数处理。
var ErrTimeout = errors.New("model call timed out")

// ErrStalled 表示流式响应在 StallTimeout 内没有任何数据：连接多半已断开而未被察觉。尚未输出任何内容时可以重试。
var ErrStalled = errors.New("model stream stalled")

// Limits 是对一个提供商的调用约束。零值表示不限制、不重试。
type Limits struct {
	// Timeout 是一次调用（不含排队与重试等待）的时限。
	Timeout time.Duration
	// MaxConcurrency 是同时进行的调用上限。
	MaxConcurrency int
	// RPS 是每秒发起调用的上限（令牌桶，突发量为一秒的量）。
	RPS float64
	// Retries 是可重试失败的重试次数；Backoff 是首次重试前的等待，之后翻倍，不超过 MaxBackoff。
	// 提供商给出 Retry-After 时取较大者（同样不超过 MaxBackoff）。
	Retries             int
	Backoff, MaxBackoff time.Duration
}

// Guard 以 Limits 包装一个提供商。
type Guard struct {
	Name     string
	Provider Provider
	Limits   Limits

	once  sync.Once
	gate  *admission
	rate  *tokenBucket
	sleep func(context.Context, time.Duration) error
}

var (
	_ Provider = (*Guard)(nil)
	_ Embedder = (*Guard)(nil)
)

func (g *Guard) init() {
	g.once.Do(func() {
		if g.Limits.MaxConcurrency > 0 {
			g.gate = &admission{limit: g.Limits.MaxConcurrency}
		}
		if g.Limits.RPS > 0 {
			g.rate = &tokenBucket{rate: g.Limits.RPS, burst: max(g.Limits.RPS, 1)}
		}
		if g.sleep == nil {
			g.sleep = sleepCtx
		}
	})
}

type backgroundKey struct{}

// WithBackground 标记 ctx 中的模型调用来自长任务（ADR-0029）：准入排队时让交互调用先行。
func WithBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

func background(ctx context.Context) bool { b, _ := ctx.Value(backgroundKey{}).(bool); return b }

func (g *Guard) Generate(ctx context.Context, req *Request, onDelta func(Delta)) (*Response, error) {
	var resp *Response
	err := g.do(ctx, req.Model, func(ctx context.Context) (bool, error) {
		emitted := false
		var cb func(Delta)
		if onDelta != nil {
			cb = func(d Delta) { emitted = true; onDelta(d) }
		}
		var err error
		resp, err = g.Provider.Generate(ctx, req, cb)
		// 已经输出过增量时不重试：订阅者已经看到了这次的部分输出。
		return !emitted, err
	})
	return resp, err
}

func (g *Guard) Embed(ctx context.Context, modelID string, texts []string) ([][]float32, error) {
	e, ok := g.Provider.(Embedder)
	if !ok {
		return nil, fmt.Errorf("model provider %q does not support embeddings", g.Name)
	}
	var vecs [][]float32
	err := g.do(ctx, modelID, func(ctx context.Context) (bool, error) {
		var err error
		vecs, err = e.Embed(ctx, modelID, texts)
		return true, err
	})
	return vecs, err
}

// do 执行 call，按需排队、限时与重试。call 返回这次失败是否可以安全重试（例如尚未输出任何内容）。
func (g *Guard) do(ctx context.Context, modelID string, call func(context.Context) (bool, error)) error {
	g.init()
	for attempt := 0; ; attempt++ {
		safe, err := g.attempt(ctx, modelID, call)
		if err == nil || ctx.Err() != nil || !safe || attempt >= g.Limits.Retries || !Retryable(err) {
			return err
		}
		wait := g.backoff(attempt, err)
		metrics.ModelRetries.WithLabelValues(g.Name).Inc()
		if err := g.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

func (g *Guard) attempt(ctx context.Context, modelID string, call func(context.Context) (bool, error)) (bool, error) {
	start := time.Now()
	if g.rate != nil {
		if err := g.rate.wait(ctx, g.sleep); err != nil {
			return false, err
		}
	}
	if g.gate != nil {
		if err := g.gate.acquire(ctx, background(ctx)); err != nil {
			return false, err
		}
		defer g.gate.release()
	}
	metrics.ModelAdmissionWait.WithLabelValues(g.Name).Observe(metrics.Since(start))
	actx := ctx
	if g.Limits.Timeout > 0 {
		var cancel context.CancelFunc
		actx, cancel = context.WithTimeout(ctx, g.Limits.Timeout)
		defer cancel()
	}
	safe, err := call(actx)
	if err != nil && ctx.Err() == nil && errors.Is(actx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("model %s: %w after %s", modelID, ErrTimeout, g.Limits.Timeout)
	}
	return safe, err
}

// backoff 是第 attempt 次失败后的等待：指数退避加抖动（避免同时失败的调用一齐重试），参考 Retry-After。
func (g *Guard) backoff(attempt int, err error) time.Duration {
	base := g.Limits.Backoff
	if base <= 0 {
		base = time.Second
	}
	ceiling := g.Limits.MaxBackoff
	if ceiling <= 0 {
		ceiling = 30 * time.Second
	}
	d := min(base<<attempt, ceiling)
	d = d/2 + rand.N(d/2+1) // 抖动：[d/2, d]
	var pe *ProviderError
	if errors.As(err, &pe) && pe.RetryAfter > d {
		d = min(pe.RetryAfter, ceiling)
	}
	return d
}

// Retryable 报告失败是否是暂时的：限流、服务端错误、连接中断、流停滞。上下文超长、参数错误、鉴权失败、
// 超时与取消不重试。
func Retryable(err error) bool {
	switch {
	case err == nil, errors.Is(err, ErrContextOverflow), errors.Is(err, ErrTimeout),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	case errors.Is(err, ErrStalled), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.EPIPE):
		return true
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		// Status 为 0 是流中途报告的错误：此时尚无输出（否则不会重试），按暂时性错误处理。
		return pe.Status == 0 || slices.Contains([]int{408, 409, 425, 429, 500, 502, 503, 504, 529}, pe.Status)
	}
	var ne net.Error
	return errors.As(err, &ne)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// admission 是带优先级的计数信号量：名额释放时先交给等待中的交互调用，再交给长任务的调用，各自先来先得。
type admission struct {
	mu       sync.Mutex
	limit    int
	inflight int
	waiting  [2][]chan struct{} // 0：交互，1：长任务
}

func (a *admission) acquire(ctx context.Context, bg bool) error {
	q := 0
	if bg {
		q = 1
	}
	a.mu.Lock()
	if a.inflight < a.limit && len(a.waiting[0])+len(a.waiting[1]) == 0 {
		a.inflight++
		a.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	a.waiting[q] = append(a.waiting[q], ch)
	a.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		a.mu.Lock()
		i := slices.Index(a.waiting[q], ch)
		if i >= 0 {
			a.waiting[q] = slices.Delete(a.waiting[q], i, i+1)
		}
		a.mu.Unlock()
		if i < 0 {
			a.release() // 放弃等待的同时被分到了名额：交还它
		}
		return ctx.Err()
	}
}

// release 交还名额：有人等待时直接转交（在途数不变），否则在途数减一。
func (a *admission) release() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for q := range a.waiting {
		if len(a.waiting[q]) > 0 {
			ch := a.waiting[q][0]
			a.waiting[q] = a.waiting[q][1:]
			close(ch)
			return
		}
	}
	a.inflight--
}

// tokenBucket 限制发起调用的速率。
type tokenBucket struct {
	mu          sync.Mutex
	rate, burst float64
	tokens      float64
	last        time.Time
}

func (b *tokenBucket) wait(ctx context.Context, sleep func(context.Context, time.Duration) error) error {
	for {
		b.mu.Lock()
		now := time.Now()
		if b.last.IsZero() {
			b.tokens = b.burst
		} else {
			b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
		}
		b.last = now
		if b.tokens >= 1 {
			b.tokens--
			b.mu.Unlock()
			return nil
		}
		need := time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
		b.mu.Unlock()
		if err := sleep(ctx, need); err != nil {
			return err
		}
	}
}
