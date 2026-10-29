package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"persona/domain"
	"persona/mcpserver"
	"xinli/kernel"
)

const DayAgent = "xinli-day"

// 单独的接缝保留已有归集客户端；只有启用删除编排时才要求这些公开 HTTP 操作。
type DeletionKernel interface {
	CreateSessionWithOptions(context.Context, string, string, string, kernel.SessionOptions) (string, error)
	Session(context.Context, string) (kernel.Session, error)
	Interrupt(context.Context, string, string) error
	DeleteSession(context.Context, string, string) error
	Deletion(context.Context, string, string) (kernel.Deletion, error)
	MemoriesBySource(context.Context, string, string, []string) ([]kernel.Memory, error)
	DeleteSessionMemories(context.Context, string, string) error
}

var _ mcpserver.DeletionNotifier = (*Orchestrator)(nil)

type retryKey struct {
	owner
	task string
}
type retryState struct {
	next  time.Time
	delay time.Duration
}

// StartDeletions 的首次扫描不依赖通知，覆盖提交擦除后、发送通知前的崩溃。
func (o *Orchestrator) StartDeletions() {
	o.mu.Lock()
	if o.closed || o.startedRetries {
		o.mu.Unlock()
		return
	}
	o.startedRetries = true
	o.pending.Add(1)
	o.mu.Unlock()
	go func() {
		defer o.pending.Done()
		ticker := time.NewTicker(o.retryInterval)
		defer ticker.Stop()
		for {
			if err := o.ScanDeletions(o.ctx); err != nil {
				o.failed("deletion_scan", "", "", "", err)
			}
			select {
			case <-o.stopRetries:
				return
			case <-o.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// ScanDeletions 同步执行一轮，测试与手动恢复可直接驱动，不用等待定时器。
func (o *Orchestrator) ScanDeletions(ctx context.Context) error {
	if !o.begin() {
		return errors.New("orchestrator: shutting down")
	}
	defer o.pending.Done()
	owners, err := o.store.PendingDeletions(ctx)
	if err != nil {
		return err
	}
	var result error
	for _, key := range owners {
		if err := o.processDeletions(ctx, key.BusinessLine, key.EndUser); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (o *Orchestrator) DeletionPending(_ context.Context, bl, eu, _ string) {
	if !o.begin() {
		return
	}
	go func() {
		defer o.pending.Done()
		if err := o.processDeletions(o.ctx, bl, eu); err != nil {
			o.failed("deletion", bl, "", "", err)
		}
	}()
}

func (o *Orchestrator) processDeletions(ctx context.Context, bl, eu string) error {
	release, err := o.acquire(ctx, bl, eu)
	if err != nil {
		return err
	}
	defer release()
	k, ok := o.kernel.(DeletionKernel)
	if !ok {
		return errors.New("orchestrator: deletion kernel unavailable")
	}
	var tasks []domain.DeletionTask
	if err := o.store.View(ctx, bl, eu, func(b *domain.Book) error { tasks = b.PendingDeletionTasks(); return nil }); err != nil {
		return err
	}
	for _, task := range tasks {
		key := retryKey{owner{bl, eu}, task.ID}
		o.mu.Lock()
		retry := o.retries[key]
		o.mu.Unlock()
		if o.now().Before(retry.next) {
			return nil
		}
		ready, class, err := o.executeDeletion(ctx, k, bl, eu, task)
		if err != nil {
			// 错误类别只描述失败步骤；下游文字永远不进入 Persona 或日志。
			saveErr := o.store.Update(ctx, bl, eu, func(b *domain.Book) error { return b.FailDeletionTask(task.ID, class, o.now()) })
			o.failed("deletion_"+class, bl, task.Session, task.ID, err)
			if saveErr != nil {
				return saveErr
			}
		}
		o.mu.Lock()
		if ready && err == nil {
			delete(o.retries, key)
		} else {
			delay := o.retryInterval
			if err != nil && retry.delay > 0 {
				delay = min(retry.delay*2, o.retryMax)
			}
			o.retries[key] = retryState{next: o.now().Add(delay), delay: delay}
		}
		o.mu.Unlock()
		// 同一业务日的后续删除必须等待前面的清理，避免旧 Run 迟到写入 Memory。
		if !ready || err != nil {
			return err
		}
	}
	return nil
}

func (o *Orchestrator) executeDeletion(ctx context.Context, k DeletionKernel, bl, eu string, task domain.DeletionTask) (bool, string, error) {
	if task.Status == "pending" {
		if task.Session != "" {
			state, err := k.Session(ctx, task.Session)
			if err != nil && !errors.Is(err, kernel.ErrNotFound) {
				return false, "session", err
			}
			if err == nil && state.ActiveRun() != "" {
				if err := k.Interrupt(ctx, task.Session, state.ActiveRun()); err != nil && !errors.Is(err, kernel.ErrNotFound) {
					return false, "interrupt", err
				}
			}
			missing := errors.Is(err, kernel.ErrNotFound)
			reason := ""
			if task.Kind == "erase" {
				reason = "erasure"
			}
			deleteErr := k.DeleteSession(ctx, task.Session, reason)
			if err := deleteErr; err != nil && !errors.Is(err, kernel.ErrNotFound) {
				// 某些入口对重复删除返回冲突；只有已有记录才能证明删除已被接受。
				if _, probeErr := k.Deletion(ctx, bl, task.Session); probeErr != nil {
					return false, "delete_session", err
				}
			}
			deletion, err := k.Deletion(ctx, bl, task.Session)
			absent := missing && errors.Is(deleteErr, kernel.ErrNotFound) && errors.Is(err, kernel.ErrNotFound)
			if err != nil && !absent {
				return false, "deletion", err
			}
			if !absent && deletion.CompletedAt.IsZero() {
				return false, "", nil
			}
		}
		candidates := []domain.MemoryCandidate{}
		if task.Kind == "erase" {
			memories, err := k.MemoriesBySource(ctx, bl, eu, task.Sessions)
			if err != nil {
				return false, "memories", err
			}
			for _, m := range memories {
				candidates = append(candidates, domain.MemoryCandidate{ID: m.ID, Summary: m.Content})
			}
		} else {
			for _, sid := range task.Sessions {
				// 404 不证明 Memory 已删除：擦除原因会保留 Memory，必须得到成功响应。
				if err := k.DeleteSessionMemories(ctx, bl, sid); err != nil {
					return false, "delete_memories", err
				}
			}
		}
		rebuild := task.Kind == "erase" && o.rebuildDue(task)
		if err := o.store.Update(ctx, bl, eu, func(b *domain.Book) error {
			return b.CompleteDeletionWithRebuild(task.ID, candidates, rebuild, o.now())
		}); err != nil {
			return false, "complete", err
		}
		task.RebuildPending = rebuild
	}
	if task.RebuildPending {
		// 更新的擦除或删一天已接管该日；旧待办不能在它们之后重新提交整理。
		skip := !o.rebuildDue(task)
		if err := o.store.View(ctx, bl, eu, func(b *domain.Book) error {
			b.SetCaptureClock(o.now)
			skip = skip || b.CheckCaptures(task.Date) != nil
			seen := false
			for _, other := range b.DeletionTasks() {
				if other.ID == task.ID {
					seen = true
					continue
				}
				if seen && other.Date == task.Date {
					skip = true
				}
			}
			return nil
		}); err != nil {
			return false, "rebuild_read", err
		}
		if !skip {
			sid, err := o.ensureDaySession(ctx, k, bl, eu, task.Date)
			if err != nil {
				return false, "rebuild", err
			}
			if err := o.kernel.Submit(ctx, sid, "xinli_rebuild_"+task.ID, "整理今天"); err != nil {
				return false, "submit", err
			}
		}
		if err := o.store.Update(ctx, bl, eu, func(b *domain.Book) error { return b.CompleteDeletionRebuild(task.ID) }); err != nil {
			return false, "rebuild_complete", err
		}
	}
	return true, "", nil
}

func (o *Orchestrator) rebuildDue(task domain.DeletionTask) bool {
	zone := o.defaultZone
	if task.TimeZone != "" {
		if z, err := time.LoadLocation(task.TimeZone); err == nil {
			zone = z
		}
	}
	now := o.now()
	return task.Date < now.In(zone).Format(time.DateOnly) && (task.DeleteAt.IsZero() || task.DeleteAt.After(now))
}

func (o *Orchestrator) EnsureDaySession(ctx context.Context, bl, eu, date string) (string, error) {
	if !o.begin() {
		return "", errors.New("orchestrator: shutting down")
	}
	defer o.pending.Done()
	release, err := o.acquire(ctx, bl, eu)
	if err != nil {
		return "", err
	}
	defer release()
	k, ok := o.kernel.(DeletionKernel)
	if !ok {
		return "", errors.New("orchestrator: deletion kernel unavailable")
	}
	return o.ensureDaySession(ctx, k, bl, eu, date)
}

func (o *Orchestrator) ensureDaySession(ctx context.Context, k DeletionKernel, bl, eu, date string) (string, error) {
	if _, err := time.Parse(time.DateOnly, date); err != nil {
		return "", errors.New("orchestrator: invalid date")
	}
	var day domain.BusinessDay
	var generation int
	read := func(b *domain.Book) error {
		b.SetCaptureClock(o.now)
		if err := b.CheckCaptures(date); err != nil {
			return err
		}
		day = b.BusinessDay(date)
		if day.Deleted {
			return domain.ErrDayDeleted
		}
		generation = 0
		for _, task := range b.DeletionTasks() {
			if task.Date == date {
				generation++
				if task.Status == "pending" {
					return errors.New("orchestrator: deletion pending")
				}
			}
		}
		return nil
	}
	if err := o.store.View(ctx, bl, eu, read); err != nil {
		return "", err
	}
	if day.ExpiresAt.IsZero() {
		return "", errors.New("orchestrator: business day has no capture deadline")
	}
	if day.Session != "" {
		return day.Session, nil
	}
	if !day.ExpiresAt.IsZero() && !day.ExpiresAt.After(o.now()) {
		return "", domain.ErrCapturesExpired
	}
	sid, err := k.CreateSessionWithOptions(ctx, bl, eu, DayAgent, kernel.SessionOptions{DeleteAt: day.ExpiresAt})
	if err != nil {
		return "", err
	}
	before := generation
	createdDeleteAt := day.ExpiresAt
	// 创建和绑定之间崩溃可能留下空 Session；它没有原文。绑定成功后重试总是复用 Persona 的当前绑定。
	err = o.store.Update(ctx, bl, eu, func(b *domain.Book) error {
		if err := read(b); err != nil {
			return err
		}
		if generation != before || !day.ExpiresAt.Equal(createdDeleteAt) {
			return errors.New("orchestrator: business day changed")
		}
		if day.Session != "" {
			sid = day.Session
			return nil
		}
		if !day.ExpiresAt.IsZero() && !day.ExpiresAt.After(o.now()) {
			return errors.New("orchestrator: business day expired")
		}
		return b.BindDay(date, sid, day.ExpiresAt)
	})
	if err != nil {
		return "", fmt.Errorf("orchestrator: bind day: %w", err)
	}
	return sid, nil
}
