// Package orchestrator 在应用层驱动跨 Session 归集，通知暂代未来的出站流。
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
)

const ProjectAgent = "xinli-project"
const WeekAgent = "xinli-week"

type Kernel interface {
	CreateSession(context.Context, string, string, string) (string, error)
	Submit(context.Context, string, string, string) error
}

type Options struct {
	Now    func() time.Time
	Logger *slog.Logger
}

type owner struct{ businessLine, endUser string }
type Orchestrator struct {
	store   store.Store
	kernel  Kernel
	ctx     context.Context
	now     func() time.Time
	logger  *slog.Logger
	mu      sync.Mutex
	closed  bool
	pending sync.WaitGroup
	locks   map[owner]chan struct{}
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
	return &Orchestrator{store: s, kernel: k, ctx: ctx, now: opts.Now, logger: opts.Logger, locks: make(map[owner]chan struct{})}
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
	o.closed = true
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

func (o *Orchestrator) DayFiled(_ context.Context, bl, eu, daySession, date string, projects []string) {
	if !o.begin() {
		return
	}
	projects = append([]string(nil), projects...)
	go func() {
		defer o.pending.Done()
		release, err := o.acquire(o.ctx, bl, eu)
		if err != nil {
			o.failed("collect", bl, daySession, "", err)
			return
		}
		defer release()
		for _, id := range projects {
			if err := o.collect(bl, eu, daySession, date, id); err != nil {
				o.failed("collect", bl, daySession, id, err)
			}
		}
	}()
}

func (o *Orchestrator) collect(bl, eu, daySession, date, id string) error {
	var found bool
	var sid string
	err := o.store.View(o.ctx, bl, eu, func(b *domain.Book) error {
		for _, p := range b.State().Projects {
			if p.ID == id {
				found = true
				sid = p.Session
				break
			}
		}
		return nil
	})
	if err != nil || !found {
		return err
	}
	if sid == "" {
		sid, err = o.kernel.CreateSession(o.ctx, bl, eu, ProjectAgent)
		if err != nil {
			return err
		}
		if err = o.store.Update(o.ctx, bl, eu, func(b *domain.Book) error { b.Bind(id, sid); return nil }); err != nil {
			return err
		}
	}
	return o.kernel.Submit(o.ctx, sid, "xinli_"+daySession+"_"+id, "整理 "+date)
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
	err = o.store.Update(ctx, bl, eu, func(b *domain.Book) error { b.Archive(now); plan = b.ReviewWeek(now); return nil })
	if err != nil {
		return "", err
	}
	if plan.Session == "" {
		plan.Session, err = o.kernel.CreateSession(ctx, bl, eu, WeekAgent)
		if err != nil {
			return "", err
		}
		if err = o.store.Update(ctx, bl, eu, func(b *domain.Book) error { b.BindWeek(plan.From, plan.Session); return nil }); err != nil {
			return "", err
		}
	}
	if err = o.kernel.Submit(ctx, plan.Session, "xinli_week_"+plan.From, "回顾本周"); err != nil {
		return "", err
	}
	sid := plan.Session
	// 提交回顾之后重新读，与旧实现一致，纳入这期间发生的领域变更。
	if err = o.store.View(ctx, bl, eu, func(b *domain.Book) error { plan = b.ReviewWeek(now); return nil }); err != nil {
		return "", err
	}
	for _, p := range plan.Rewrite {
		if err := o.kernel.Submit(ctx, p.Session, "xinli_rewrite_"+plan.From+"_"+p.ID, "重写现状"); err != nil {
			o.failed("rewrite", bl, p.Session, p.ID, err)
		}
	}
	return sid, nil
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
