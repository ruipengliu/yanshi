package orchestrator

import (
	"context"
	"errors"
	"time"

	"persona/domain"
	"persona/mcpserver"
	"xinli/kernel"
	"xinli/task"
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

func (o *Orchestrator) processDeletionsActive(ctx context.Context, bl, eu string) error {
	release, err := o.acquire(ctx, bl, eu)
	if err != nil {
		return err
	}
	defer release()
	k, _ := o.kernel.(DeletionKernel)
	var tasks []domain.DeletionTask
	if err := o.store.View(ctx, bl, eu, func(b *domain.Book) error { tasks = b.DeletionTasks(); return nil }); err != nil {
		return err
	}
	for _, deletion := range tasks {
		source := deletionSource(bl, eu, deletion.ID, "delete")
		spec := task.Spec{Operation: "delete", Target: "deletions", Date: deletion.Date}
		if err := o.tasks.Receive(ctx, source, []task.Spec{spec}, o.now()); err != nil {
			return err
		}
		job, claimed, err := o.tasks.Claim(ctx, task.Identity(source, spec), o.now(), 2*time.Minute)
		if err != nil {
			return err
		}
		if !claimed {
			current, err := o.tasks.Get(ctx, task.Identity(source, spec))
			if err != nil {
				return err
			}
			if current.State == task.Succeeded {
				continue
			}
			return nil
		}
		callCtx, cancel := context.WithTimeout(ctx, max(time.Nanosecond, job.LeaseUntil.Sub(o.now())))
		ready := true
		class := ""
		var callErr error
		if deletion.Status == "pending" || deletion.RebuildPending {
			if k == nil {
				ready, class, callErr = false, "configuration", task.ErrConfiguration
			} else {
				ready, class, callErr = o.executeDeletion(callCtx, k, bl, eu, deletion)
			}
		}
		cancel()
		if callErr != nil {
			// 只保存步骤类别，下游错误正文不进入领域存储或日志。
			saveErr := o.store.Update(ctx, bl, eu, func(b *domain.Book) error { return b.FailDeletionTask(deletion.ID, class, o.now()) })
			callErr = errors.Join(callErr, saveErr)
			o.failed("deletion_"+class, bl, deletion.Session, deletion.ID, callErr)
		}
		job.LastErrorClass = class
		job.RetryAt = o.now().Add(task.Backoff(job.TaskID, job.Attempt, o.retryInterval, o.retryMax))
		if ready && callErr == nil {
			job.State = task.Succeeded
		}
		if task.IsConfiguration(callErr) {
			job.State = task.Failed
			job.LastErrorClass = "configuration"
		}
		if err := o.tasks.Save(ctx, job, o.now(), true); err != nil {
			return errors.Join(callErr, err)
		}
		// 同一 owner 的后续删除必须等前项清理，不提前读取可能还有迟到写入的 Memory。
		if !ready || callErr != nil {
			return callErr
		}
	}
	return nil
}

func (o *Orchestrator) executeDeletion(ctx context.Context, k DeletionKernel, bl, eu string, deletionTask domain.DeletionTask) (bool, string, error) {
	if deletionTask.Status == "pending" {
		var sessions []string
		if err := o.store.View(ctx, bl, eu, func(b *domain.Book) error {
			sessions = b.DeletionSessionIDs(deletionTask)
			return nil
		}); err != nil {
			return false, "sessions", err
		}
		reason := ""
		if deletionTask.Kind == "erase" {
			reason = "erasure"
		}
		for _, sid := range sessions {
			if ready, class, err := deleteWorkSession(ctx, k, bl, sid, reason); err != nil || !ready {
				return ready, class, err
			}
		}
		candidates := []domain.MemoryCandidate{}
		if deletionTask.Kind == "erase" {
			memories, err := k.MemoriesBySource(ctx, bl, eu, deletionTask.Sessions)
			if err != nil {
				return false, "memories", err
			}
			for _, m := range memories {
				candidates = append(candidates, domain.MemoryCandidate{ID: m.ID, Summary: m.Content})
			}
		} else {
			for _, sid := range deletionTask.Sessions {
				// 404 不证明 Memory 已删除：擦除原因会保留 Memory，必须得到成功响应。
				if err := k.DeleteSessionMemories(ctx, bl, sid); err != nil {
					return false, "delete_memories", err
				}
			}
		}
		rebuild := deletionTask.Kind == "erase" && o.rebuildDue(deletionTask)
		if err := o.store.Update(ctx, bl, eu, func(b *domain.Book) error {
			return b.CompleteDeletionWithRebuild(deletionTask.ID, candidates, rebuild, o.now())
		}); err != nil {
			return false, "complete", err
		}
		deletionTask.RebuildPending = rebuild
	}
	if deletionTask.RebuildPending {
		skip, err := o.skipRebuild(ctx, bl, eu, deletionTask)
		if err != nil {
			return false, "rebuild_read", err
		}
		if !skip {
			job, err := o.rebuildTask(ctx, bl, eu, deletionTask)
			if err != nil {
				if errors.Is(err, errRebuildPreparation) {
					return false, "rebuild", err
				}
				return false, "submit", err
			}
			if job.State == task.Failed {
				return false, "submit", task.ErrConfiguration
			}
			if job.State != task.Accepted && job.State != task.Succeeded && job.State != task.Terminated {
				return false, "", nil
			}
		}
		if err := o.store.Update(ctx, bl, eu, func(b *domain.Book) error {
			return b.CompleteDeletionRebuild(deletionTask.ID)
		}); err != nil {
			return false, "rebuild_complete", err
		}
	}
	return true, "", nil
}

// 更新的擦除、删一天或自然到期已接管该日；旧待办不能重新提交整理。
func (o *Orchestrator) skipRebuild(ctx context.Context, bl, eu string, deletion domain.DeletionTask) (bool, error) {
	skip := !o.rebuildDue(deletion)
	err := o.store.View(ctx, bl, eu, func(b *domain.Book) error {
		b.SetCaptureClock(o.now)
		skip = skip || b.CheckCaptures(deletion.Date) != nil
		seen := false
		for _, other := range b.DeletionTasks() {
			if other.ID == deletion.ID {
				seen = true
				continue
			}
			if seen && other.Date == deletion.Date {
				skip = true
			}
		}
		return nil
	})
	return skip, err
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

func (o *Orchestrator) ensureDaySessionActive(ctx context.Context, bl, eu, date string) (string, error) {
	if !o.begin() {
		return "", errors.New("orchestrator: shutting down")
	}
	defer o.pending.Done()
	release, err := o.acquire(ctx, bl, eu)
	if err != nil {
		return "", err
	}
	defer release()
	return o.ensureDay(ctx, bl, eu, date)
}

func deleteWorkSession(ctx context.Context, k DeletionKernel, bl, sid, reason string) (bool, string, error) {
	ctx = kernel.WithBusinessLine(ctx, bl)
	state, err := k.Session(ctx, sid)
	if err != nil && !errors.Is(err, kernel.ErrNotFound) {
		return false, "session", err
	}
	if err == nil && state.ActiveRun() != "" {
		if err := k.Interrupt(ctx, sid, state.ActiveRun()); err != nil && !errors.Is(err, kernel.ErrNotFound) {
			return false, "interrupt", err
		}
	}
	missing := errors.Is(err, kernel.ErrNotFound)
	deleteErr := k.DeleteSession(ctx, sid, reason)
	if err := deleteErr; err != nil && !errors.Is(err, kernel.ErrNotFound) {
		// 某些入口对重复删除返回冲突；只有已有记录才能证明删除已被接受。
		if _, probeErr := k.Deletion(ctx, bl, sid); probeErr != nil {
			return false, "delete_session", err
		}
	}
	deletion, err := k.Deletion(ctx, bl, sid)
	absent := missing && errors.Is(deleteErr, kernel.ErrNotFound) && errors.Is(err, kernel.ErrNotFound)
	if err != nil && !absent {
		return false, "deletion", err
	}
	if !absent && deletion.CompletedAt.IsZero() {
		return false, "", nil
	}
	return true, "", nil
}

func (o *Orchestrator) processDeletions(ctx context.Context, bl, eu string) error {
	return o.withActive(ctx, task.Owner{BusinessLine: bl, EndUser: eu}, func(ctx context.Context) error { return o.processDeletionsActive(ctx, bl, eu) })
}
func (o *Orchestrator) EnsureDaySession(ctx context.Context, bl, eu, date string) (string, error) {
	var out string
	err := o.withActive(ctx, task.Owner{BusinessLine: bl, EndUser: eu}, func(ctx context.Context) error {
		var err error
		out, err = o.ensureDaySessionActive(ctx, bl, eu, date)
		return err
	})
	return out, err
}
