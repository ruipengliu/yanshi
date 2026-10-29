package orchestrator

import (
	"context"
	"errors"
	"time"

	"persona/domain"
	"persona/store"
	"xinli/kernel"
	"xinli/task"
)

func workLease(t task.Task) domain.WorkLease {
	return domain.WorkLease{Task: t.TaskID, Sequence: t.Sequence, Generation: t.LeaseGeneration, Until: t.LeaseUntil}
}

func (o *Orchestrator) ensureWork(ctx context.Context, t task.Task, agent string) (string, error) {
	var expected domain.WorkTarget
	err := o.store.Update(ctx, t.BusinessLine, t.EndUser, func(b *domain.Book) error {
		b.SetCaptureClock(o.now)
		var err error
		expected, err = b.ReserveWork(t.Target, workLease(t), o.now())
		return err
	})
	if err != nil {
		return "", err
	}
	if expected.Session != "" {
		previous, held, err := o.tasks.SessionTask(ctx, t.Owner, expected.Session)
		if err != nil {
			return "", err
		}
		if !held || !previous.Terminal() {
			return expected.Session, nil
		}
		// A8 留下的 failed/interrupted 槽位也须退役，不能靠新任务的租约清掉旧资格。
		err = o.store.Update(ctx, t.BusinessLine, t.EndUser, func(b *domain.Book) error {
			b.SetCaptureClock(o.now)
			current, err := b.ReserveWork(t.Target, workLease(t), o.now())
			if err != nil {
				return err
			}
			if current.Session != expected.Session || current.Version != expected.Version {
				return domain.ErrWorkConflict
			}
			if err := b.RetireWork(t.Target, expected.Session, o.now()); err != nil {
				return err
			}
			expected, err = b.ReserveWork(t.Target, workLease(t), o.now())
			return err
		})
		if err != nil {
			return "", err
		}
	}
	now := o.now()
	intent := task.Creation{Owner: t.Owner, ID: task.CreationID(t), TaskID: t.TaskID, Target: t.Target, Agent: agent,
		Generation: t.LeaseGeneration, Expected: expected.Session, Version: expected.Version, Revision: expected.Revision,
		DeleteAt: expected.DeleteAt, CreatedAt: now, LeaseUntil: t.LeaseUntil}
	if err := o.tasks.BeginCreation(ctx, t, intent, now); err != nil {
		return "", err
	}
	var sid string
	if expected.DeleteAt.IsZero() {
		sid, err = o.kernel.CreateSession(ctx, t.BusinessLine, t.EndUser, agent)
	} else if k, ok := o.kernel.(interface {
		CreateSessionWithOptions(context.Context, string, string, string, kernel.SessionOptions) (string, error)
	}); ok {
		sid, err = k.CreateSessionWithOptions(ctx, t.BusinessLine, t.EndUser, agent, kernel.SessionOptions{DeleteAt: expected.DeleteAt})
	} else {
		return "", task.ErrConfiguration
	}
	if err != nil {
		return "", err
	}
	// 即使租约已失效也保存返回的 ID，交给孤儿扫描；此处不授予任何读写资格。
	if err := o.tasks.RecordCreated(ctx, intent.ID, sid); err != nil {
		return "", err
	}
	if err := o.store.Update(ctx, t.BusinessLine, t.EndUser, func(b *domain.Book) error {
		b.SetCaptureClock(o.now)
		return b.BindWork(t.Target, expected, workLease(t), sid, o.now())
	}); err != nil {
		return "", err
	}
	if err := o.tasks.ForgetCreation(ctx, intent.ID); err != nil {
		return "", err
	}
	return sid, nil
}

func commandKind(t task.Task) string {
	if t.Operation == "rebuild" {
		return "day"
	}
	return t.Operation
}

func (o *Orchestrator) registerCommand(ctx context.Context, t task.Task) error {
	return o.store.Update(ctx, t.BusinessLine, t.EndUser, func(b *domain.Book) error {
		b.SetCaptureClock(o.now)
		// 重新校验租约与当前绑定，不把 Prepare 的快照当作提交资格。
		if _, err := b.ReserveWork(t.Target, workLease(t), o.now()); err != nil {
			return err
		}
		return b.RegisterWorkCommand(t.Target, t.Session, t.InputID, commandKind(t), workLease(t), o.now())
	})
}

func (o *Orchestrator) finishCommand(ctx context.Context, t task.Task) error {
	return o.store.Update(ctx, t.BusinessLine, t.EndUser, func(b *domain.Book) error {
		return b.FinishWorkCommand(t.Session, t.InputID)
	})
}

func (o *Orchestrator) retireCommand(ctx context.Context, t task.Task) error {
	err := o.store.Update(ctx, t.BusinessLine, t.EndUser, func(b *domain.Book) error {
		b.SetCaptureClock(o.now)
		if !t.LeaseUntil.After(o.now()) {
			return task.ErrLeaseLost
		}
		// 新持有者先登记 fence；旧持有者既不能覆盖它，也不能退役其复用的绑定。
		if _, err := b.ReserveWork(t.Target, workLease(t), o.now()); err != nil {
			if errors.Is(err, domain.ErrDayDeleted) || errors.Is(err, domain.ErrCapturesExpired) || errors.Is(err, domain.ErrWorkBusy) || errors.Is(err, domain.ErrWorkTargetGone) {
				return b.RetireWork(t.Target, t.Session, o.now())
			}
			return err
		}
		return b.RetireWork(t.Target, t.Session, o.now())
	})
	if errors.Is(err, store.ErrOwnerDeleting) {
		return nil
	}
	return err
}

// DayInput 与删除后的重建共用任务执行器；入口不能先提交文字再补登记。
func (o *Orchestrator) dayInputActive(ctx context.Context, bl, eu, date, kind, inputID string) (string, error) {
	if kind != "day" && kind != "exchange" || inputID == "" {
		return "", task.ErrConfiguration
	}
	if !o.begin() {
		return "", errors.New("orchestrator: shutting down")
	}
	defer o.pending.Done()
	source := task.Source{Owner: task.Owner{BusinessLine: bl, EndUser: eu}, Kind: "day_request", ID: "day_request/" + inputID, Version: 1}
	spec := task.Spec{Operation: kind, Target: "day/" + date, Date: date}
	if err := o.tasks.Receive(ctx, source, []task.Spec{spec}, o.now()); err != nil {
		return "", err
	}
	job, err := o.runTask(ctx, task.Identity(source, spec))
	if err != nil {
		return "", err
	}
	if job.State != task.Accepted && job.State != task.Succeeded {
		return "", domain.ErrWorkBusy
	}
	return job.Session, nil
}

// 无内容的按需创建也使用有租约的持久任务；每次换绑/擦除后的版本都产生新的创建身份。
func (o *Orchestrator) ensureDayActive(ctx context.Context, bl, eu, date string) (string, error) {
	var target domain.WorkTarget
	err := o.store.View(ctx, bl, eu, func(b *domain.Book) error {
		b.SetCaptureClock(o.now)
		var err error
		target, err = b.WorkTarget("day/" + date)
		return err
	})
	if err != nil {
		return "", err
	}
	if target.Session != "" {
		return target.Session, nil
	}
	source := task.Source{Owner: task.Owner{BusinessLine: bl, EndUser: eu}, Kind: "ensure", ID: "ensure/day/" + date, Version: target.Version + target.Revision + 1}
	spec := task.Spec{Operation: "ensure", Target: "day/" + date, Date: date}
	if err := o.tasks.Receive(ctx, source, []task.Spec{spec}, o.now()); err != nil {
		return "", err
	}
	job, claimed, err := o.tasks.Claim(ctx, task.Identity(source, spec), o.now(), 2*time.Minute)
	if err != nil {
		return "", err
	}
	if !claimed {
		return "", domain.ErrWorkBusy
	}
	return o.runEnsure(ctx, job)
}

func (o *Orchestrator) runEnsureActive(ctx context.Context, job task.Task) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, max(time.Nanosecond, job.LeaseUntil.Sub(o.now())))
	defer cancel()
	sid, err := o.ensureWork(callCtx, job, DayAgent)
	if err == nil {
		job.State = task.Succeeded
	} else {
		job.LastErrorClass = "create"
		if errors.Is(err, store.ErrOwnerDeleting) || errors.Is(err, domain.ErrDayDeleted) || errors.Is(err, domain.ErrCapturesExpired) {
			job.State = task.Terminated
		}
	}
	job.RetryAt = o.now().Add(task.Backoff(job.TaskID, job.Attempt, o.retryInterval, o.retryMax))
	return sid, errors.Join(err, o.tasks.Save(ctx, job, o.now(), true))
}
