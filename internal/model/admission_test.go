package model

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"yanshi/internal/metrics"
)

func classContext(class admissionClass) context.Context {
	ctx := context.Background()
	if class == backgroundRun || class == backgroundOneshot {
		ctx = WithBackground(ctx)
	}
	if class == oneshot || class == backgroundOneshot {
		ctx = WithOneshot(ctx)
	}
	return ctx
}

func TestAdmissionClassMarkers(t *testing.T) {
	for class := interactive; class < admissionClasses; class++ {
		if got := classOf(classContext(class)); got != class {
			t.Errorf("class %s became %s", class, got)
		}
	}
	ctx := WithBackground(WithOneshot(context.Background()))
	if got := classOf(ctx); got != backgroundOneshot {
		t.Fatalf("reversed markers: %s", got)
	}
}

func TestGuardRejectsInvalidMinimum(t *testing.T) {
	for _, minimum := range []int{-1, 2, 3} {
		p := &scripted{}
		g := &Guard{Provider: p, Limits: Limits{MaxConcurrency: 2, MinBackground: minimum}}
		if _, err := g.Generate(context.Background(), &Request{}, nil); err == nil || p.calls != 0 {
			t.Fatalf("minimum %d: err %v, calls %d", minimum, err, p.calls)
		}
	}
}

// 等待真实的队列状态，不用固定延迟推测 goroutine 已经排队。
func waitFor(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for queue state")
		}
		runtime.Gosched()
	}
}

func waitQueued(t *testing.T, a *admission, n int) {
	t.Helper()
	waitFor(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.queued() == n
	})
}

func assertAdmissionEmpty(t *testing.T, a *admission) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inflight != 0 || a.queued() != 0 || a.active != [admissionClasses]int{} {
		t.Fatalf("inflight %d, active %v, queued %d", a.inflight, a.active, a.queued())
	}
}

func holdAdmission(t *testing.T, a *admission, class admissionClass) func() {
	t.Helper()
	r, err := a.acquire(classContext(class), class)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type admissionResult struct {
	id      int
	release func()
	err     error
}

func queueAdmission(t *testing.T, a *admission, ctx context.Context, class admissionClass, id int, done chan<- admissionResult) {
	t.Helper()
	a.mu.Lock()
	n := a.queued()
	a.mu.Unlock()
	go func() {
		r, err := a.acquire(ctx, class)
		done <- admissionResult{id, r, err}
	}()
	waitQueued(t, a, n+1)
}

func nextAdmission(t *testing.T, done <-chan admissionResult, id int) func() {
	t.Helper()
	select {
	case got := <-done:
		if got.id != id || got.err != nil {
			t.Fatalf("got id %d, err %v; want %d", got.id, got.err, id)
		}
		return got.release
	case <-time.After(5 * time.Second):
		t.Fatal("admission did not hand off")
		return nil
	}
}

func TestAdmissionFourClassesFIFO(t *testing.T) {
	a := &admission{limit: 1}
	held := holdAdmission(t, a, interactive)
	done := make(chan admissionResult, 8)
	// 逆优先级入队，每类两位；每次交付后由测试放行下一位。
	for class := backgroundOneshot; class >= interactive; class-- {
		for i := range 2 {
			queueAdmission(t, a, classContext(class), class, int(class)*2+i, done)
		}
	}
	held()
	for id := range 8 {
		nextAdmission(t, done, id)()
	}
	assertAdmissionEmpty(t, a)
}

func TestAdmissionMinimumBackground(t *testing.T) {
	a := &admission{limit: 4, minBackground: 2}
	// 后台单次调用占一个名额，但不抵扣后台 Run 的最低份额。
	bgOneshot := holdAdmission(t, a, backgroundOneshot)
	holders := []func(){holdAdmission(t, a, interactive), holdAdmission(t, a, interactive), holdAdmission(t, a, interactive)}
	done := make(chan admissionResult, 8)
	for id := range 3 {
		queueAdmission(t, a, classContext(backgroundRun), backgroundRun, 10+id, done)
	}
	for id := range 3 {
		queueAdmission(t, a, classContext(interactive), interactive, id, done)
	}
	holders[0]()
	bg1 := nextAdmission(t, done, 10)
	holders[1]()
	bg2 := nextAdmission(t, done, 11)
	a.mu.Lock()
	active := a.active
	a.mu.Unlock()
	if active[backgroundRun] != 2 || active[backgroundOneshot] != 1 {
		t.Fatalf("active %v", active)
	}
	// 份额已足：即使还有后台 Run 排队，下一名额先给交互。
	holders[2]()
	fg1 := nextAdmission(t, done, 0)
	// 后台 Run 离开后先补回份额，等待中的交互不能越过它。
	bg1()
	bg3 := nextAdmission(t, done, 12)
	bgOneshot()
	fg2 := nextAdmission(t, done, 1)
	fg1()
	fg3 := nextAdmission(t, done, 2)
	bg2()
	bg3()
	fg2()
	fg3()
	assertAdmissionEmpty(t, a)
}

func TestAdmissionBackgroundCanFillIdleCapacity(t *testing.T) {
	a := &admission{limit: 3, minBackground: 1}
	var held []func()
	for range 3 {
		held = append(held, holdAdmission(t, a, backgroundRun))
	}
	for _, release := range held {
		release()
		release()
	}
	assertAdmissionEmpty(t, a)
}

func TestAdmissionCancelledWaiterDoesNotLeak(t *testing.T) {
	for class := interactive; class < admissionClasses; class++ {
		for _, granted := range []bool{false, true} {
			t.Run(class.String()+map[bool]string{false: "/queued", true: "/granted"}[granted], func(t *testing.T) {
				a := &admission{limit: 1}
				held := holdAdmission(t, a, interactive)
				ctx, cancel := context.WithCancel(classContext(class))
				defer cancel()
				done := make(chan admissionResult, 1)
				queueAdmission(t, a, ctx, class, 0, done)
				if granted {
					// 持锁取消再交付，保证取消清理看到的等待者已从队列移除。
					a.mu.Lock()
					cancel()
					a.releaseLocked(interactive)
					a.mu.Unlock()
				} else {
					cancel()
				}
				got := <-done
				if !errors.Is(got.err, context.Canceled) || got.release != nil {
					t.Fatalf("result %+v", got)
				}
				if !granted {
					held()
				}
				assertAdmissionEmpty(t, a)
			})
		}
	}
}

func TestGuardRetryReleasesAndPreservesClass(t *testing.T) {
	for class := interactive; class < admissionClasses; class++ {
		t.Run(class.String(), func(t *testing.T) {
			calls := 0
			var g *Guard
			p := providerFunc(func(ctx context.Context, _ *Request, _ func(Delta)) (*Response, error) {
				calls++
				g.gate.mu.Lock()
				active := g.gate.active
				g.gate.mu.Unlock()
				if classOf(ctx) != class || active[class] != 1 {
					t.Errorf("class %v, active %v", classOf(ctx), active)
				}
				if calls == 1 {
					return nil, &ProviderError{Status: 503}
				}
				return &Response{}, nil
			})
			g = &Guard{Name: "retry-class", Provider: p, Limits: Limits{MaxConcurrency: 2, MinBackground: 1, Retries: 1}}
			g.sleep = func(ctx context.Context, _ time.Duration) error {
				assertAdmissionEmpty(t, g.gate)
				// 在退避期间别的调用确实可以获得并使用全部名额。
				r1 := holdAdmission(t, g.gate, interactive)
				r2 := holdAdmission(t, g.gate, interactive)
				r1()
				r2()
				return ctx.Err()
			}
			if _, err := g.Generate(classContext(class), &Request{}, nil); err != nil || calls != 2 {
				t.Fatalf("calls %d, err %v", calls, err)
			}
			assertAdmissionEmpty(t, g.gate)
		})
	}
}

func TestGuardMixedCancellationStress(t *testing.T) {
	const count = 400
	proceed, start := make(chan struct{}), make(chan struct{})
	p := providerFunc(func(ctx context.Context, _ *Request, _ func(Delta)) (*Response, error) {
		select {
		case <-proceed:
			return &Response{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	g := &Guard{Name: "stress", Provider: p, Limits: Limits{MaxConcurrency: 8, MinBackground: 2}}
	g.init()
	var wg sync.WaitGroup
	var cancels []context.CancelFunc
	for i := range count {
		ctx, cancel := context.WithCancel(classContext(admissionClass(i % 4)))
		cancels = append(cancels, cancel)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := g.Generate(ctx, &Request{}, nil)
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}()
	}
	close(start)
	waitQueued(t, g.gate, count-8)
	for i, cancel := range cancels {
		if i%3 == 0 {
			wg.Add(1)
			go func() { defer wg.Done(); cancel() }()
		}
	}
	close(proceed)
	wg.Wait()
	for _, cancel := range cancels {
		cancel()
	}
	assertAdmissionEmpty(t, g.gate)
}

func TestGuardAdmissionMetricClasses(t *testing.T) {
	g := &Guard{Name: "metric-classes", Provider: &scripted{}}
	for class := interactive; class < admissionClasses; class++ {
		if _, err := g.Generate(classContext(class), &Request{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, family := range families {
		if family.GetName() != "yanshi_model_admission_wait_seconds" {
			continue
		}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["provider"] == g.Name {
				if len(labels) != 2 || metric.GetHistogram().GetSampleCount() == 0 {
					t.Fatalf("unexpected metric %v", metric)
				}
				got[labels["class"]] = true
			}
		}
	}
	for class := interactive; class < admissionClasses; class++ {
		if !got[class.String()] {
			t.Errorf("missing metric class %s", class)
		}
	}
}
