// Package orchestrator 在应用层持久接收领域变化，驱动跨 Session 编排。
package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"persona/domain"
	"persona/mcpserver"
	"persona/store"
	"xinli/task"
	taskmem "xinli/task/memstore"
)

const ProjectAgent = "xinli-project"
const WeekAgent = "xinli-week"

type Kernel interface {
	CreateSession(context.Context, string, string, string) (string, error)
	Submit(context.Context, string, string, string) error
}

type Options struct {
	// Tasks 由装配方注入应用独立数据库；省略仅适用于内存开发模式。
	Tasks           task.Store
	Now             func() time.Time
	Logger          *slog.Logger
	CommandTimeout  time.Duration
	OrphanGrace     time.Duration
	RetryInterval   time.Duration
	RetryMax        time.Duration
	DefaultTimeZone string
}

type owner struct{ businessLine, endUser string }
type Orchestrator struct {
	store              store.Store
	kernel             Kernel
	ctx                context.Context
	now                func() time.Time
	logger             *slog.Logger
	mu                 sync.Mutex
	closed             bool
	pending            sync.WaitGroup
	locks              map[owner]chan struct{}
	retryInterval      time.Duration
	retryMax           time.Duration
	defaultZone        *time.Location
	stopRetries        chan struct{}
	startedRetries     bool
	startedExpirations bool
	tasks              task.Store
	worker             *task.Executor
	startedTasks       bool
	startedSchedules   bool
	orphanGrace        time.Duration
}

var _ mcpserver.Notifier = (*Orchestrator)(nil)

// ctx 属于进程生命周期，不得使用某次 MCP 请求的 context。
func New(ctx context.Context, s store.Store, k Kernel, opts Options) *Orchestrator {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.RetryInterval <= 0 {
		opts.RetryInterval = 30 * time.Second
	}
	if opts.RetryMax < opts.RetryInterval {
		opts.RetryMax = max(opts.RetryInterval, 10*time.Minute)
	}
	if opts.DefaultTimeZone == "" {
		opts.DefaultTimeZone = "Asia/Shanghai"
	}
	zone, err := time.LoadLocation(opts.DefaultTimeZone)
	if err != nil {
		zone = time.FixedZone("Asia/Shanghai", 8*60*60)
	}
	if opts.Tasks == nil {
		opts.Tasks = taskmem.New()
	}
	o := &Orchestrator{retryInterval: opts.RetryInterval, retryMax: opts.RetryMax, defaultZone: zone,
		stopRetries: make(chan struct{}), tasks: opts.Tasks, store: s, kernel: k, ctx: ctx, now: opts.Now, logger: opts.Logger, locks: make(map[owner]chan struct{})}
	taskKernel, _ := k.(task.Kernel)
	o.orphanGrace = opts.OrphanGrace
	if o.orphanGrace <= 0 {
		o.orphanGrace = 10 * time.Minute
	}
	o.worker = &task.Executor{Check: o.checkScheduledTask, Complete: o.completeScheduledTask, Register: o.registerCommand, Finish: o.finishCommand, Retire: o.retireCommand, CommandTimeout: opts.CommandTimeout, Store: o.tasks, Kernel: taskKernel, Prepare: o.prepareTask, Now: o.now, RetryBase: o.retryInterval, RetryMax: o.retryMax}
	return o
}

func (o *Orchestrator) begin() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return false
	}
	o.pending.Add(1)
	return true
}

// Shutdown 先停止接收通知，再等待已接收的任务；调用方超时后可取消生命周期 context。
func (o *Orchestrator) Shutdown(ctx context.Context) error {
	o.mu.Lock()
	if !o.closed {
		o.closed = true
		close(o.stopRetries)
	}
	o.mu.Unlock()
	done := make(chan struct{})
	go func() { o.pending.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// 同一用户的编排串行化，覆盖项目绑定与每周绑定；网络请求不占用 Persona 的事务锁。
func (o *Orchestrator) acquire(ctx context.Context, bl, eu string) (func(), error) {
	o.mu.Lock()
	key := owner{bl, eu}
	lock := o.locks[key]
	if lock == nil {
		lock = make(chan struct{}, 1)
		o.locks[key] = lock
	}
	o.mu.Unlock()
	select {
	case lock <- struct{}{}:
		return func() { <-lock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// DayFiled 仅唤醒持久消费者；通知的参数不是新的领域事实，不能用它们另建任务。
func (o *Orchestrator) DayFiled(_ context.Context, bl, eu, _, _ string, _ []string) {
	if !o.begin() {
		return
	}
	go func() {
		defer o.pending.Done()
		if err := o.receiveChanges(o.ctx, bl, eu); err != nil {
			o.failed("receive", bl, "", "", err)
		}
		if err := o.scanInputs(o.ctx); err != nil {
			o.failed("collect", bl, "", "", err)
		}
	}()
}

func (o *Orchestrator) WeekFiled(ctx context.Context, bl, eu string) {
	o.DayFiled(ctx, bl, eu, "", "", nil)
}

func (o *Orchestrator) Week(ctx context.Context, bl, eu string) (string, error) {
	if !o.begin() {
		return "", errors.New("orchestrator: shutting down")
	}
	defer o.pending.Done()
	release, err := o.acquire(ctx, bl, eu)
	if err != nil {
		return "", err
	}
	defer release()
	var plan domain.WeekPlan
	now := o.now()
	err = o.store.Update(ctx, bl, eu, func(b *domain.Book) error {
		b.Archive(now)
		plan = b.ReviewWeek(now)
		return nil
	})
	if err != nil {
		return "", err
	}
	job, err := o.weekInput(ctx, bl, eu, plan.From, "week", "week/"+plan.From)
	if err != nil {
		return "", err
	}
	if job.State != task.Accepted && job.State != task.Succeeded {
		return "", errors.New("orchestrator: week submission pending or failed")
	}
	return job.Session, nil
}

func (o *Orchestrator) failed(operation, bl, sid, project string, err error) {
	// Store 与 Kernel 接口可由外部实现，错误文字不保证不含个人数据，只记录错误种类。
	kind := "operation failed"
	if errors.Is(err, context.Canceled) {
		kind = "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		kind = "deadline exceeded"
	}
	o.logger.Warn("xinli: "+operation, "business_line", bl, "session_id", sid, "project", project, "error", kind)
}
