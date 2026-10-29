package account

import (
	"context"
	"errors"
	"time"

	"persona/store"
	"xinli/kernel"
	"xinli/task"
)

type Kernel interface {
	SetTier(context.Context, string, string, string) (kernel.Tier, error)
	DeleteEndUser(context.Context, string, string) (string, error)
	EndUserDeletion(context.Context, string, string) (kernel.EndUserDeletion, error)
}
type Persona interface {
	BeginOwnerDeletion(context.Context, string, string, time.Time) (store.OwnerDeletion, error)
	Purge(context.Context, string, string, int, time.Time) (store.OwnerDeletion, error)
}

type Worker struct {
	Accounts  *Service
	Tasks     task.Store
	Persona   Persona
	Kernel    Kernel
	Lease     time.Duration
	RetryBase time.Duration
	RetryMax  time.Duration
}

// Run 每次只推进一个注销步骤；步骤结果先落库，随后才释放任务租约。
// 崩溃于二者之间时下一持有者重复幂等步骤，不能提前报告完成。
func (w *Worker) Run(ctx context.Context, id string) (task.Task, error) {
	initial, err := w.Tasks.Get(ctx, id)
	if err != nil {
		return task.Task{}, err
	}
	if !IsTask(initial) {
		return initial, task.ErrInvalid
	}
	var out task.Task
	err = w.Accounts.Store.WithOwner(ctx, initial.Owner, func(ctx context.Context) error {
		lease := w.Lease
		if lease <= 0 {
			lease = 2 * time.Minute
		}
		t, claimed, err := w.Tasks.Claim(ctx, id, w.Accounts.now(), lease)
		if err != nil {
			return err
		}
		if !claimed {
			out, err = w.Tasks.Get(ctx, id)
			return err
		}
		out = t
		r, err := w.Accounts.Store.ReadAccount(ctx, t.Owner)
		if err != nil {
			return w.retry(ctx, &out, err)
		}
		// 应用永久终态在内核屏障宽限期结束后仍阻止旧档位调用。
		if t.Operation == TierOperation {
			if r.Account.Status != Active || t.Version != r.Account.EntitlementVersion {
				out.State = task.Terminated
				return w.Tasks.Save(ctx, out, w.Accounts.now(), true)
			}
			if w.Kernel == nil {
				return w.retry(ctx, &out, task.ErrConfiguration)
			}
			callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			effective, err := w.Kernel.SetTier(callCtx, t.BusinessLine, t.EndUser, r.Account.Entitlement)
			if err != nil {
				return w.retry(ctx, &out, err)
			}
			err = w.Accounts.Store.UpdateAccount(ctx, t.Owner, func(next *Record) error {
				if next.Account.Status != Active || next.Account.EntitlementVersion != t.Version {
					return ErrInactive
				}
				next.Account.SyncedVersion = t.Version
				next.Account.EffectiveTier = effective
				next.Account.UpdatedAt = w.Accounts.now()
				return nil
			})
			if err != nil {
				return w.retry(ctx, &out, err)
			}
			out.State = task.Succeeded
			return w.Tasks.Save(ctx, out, w.Accounts.now(), true)
		}
		if r.Account.Status == Deleted {
			out.State = task.Succeeded
			return nil
		}
		if r.Account.Status != Deleting {
			return w.retry(ctx, &out, task.ErrInvalid)
		}
		next := r.Deletion
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		switch next.Stage {
		case "persona_barrier":
			if w.Persona == nil {
				return w.retry(ctx, &out, task.ErrConfiguration)
			}
			_, err = w.Persona.BeginOwnerDeletion(callCtx, t.BusinessLine, t.EndUser, w.Accounts.now())
			if err == nil {
				next.Stage = "persona_purge"
			}
		case "persona_purge":
			if w.Persona == nil {
				return w.retry(ctx, &out, task.ErrConfiguration)
			}
			var progress store.OwnerDeletion
			progress, err = w.Persona.Purge(callCtx, t.BusinessLine, t.EndUser, store.MaxPurgeBatch, w.Accounts.now())
			if err == nil && progress.Status == "completed" {
				next.Stage = "kernel_request"
			}
		case "kernel_request":
			if w.Kernel == nil {
				return w.retry(ctx, &out, task.ErrConfiguration)
			}
			next.KernelRequestID, err = w.Kernel.DeleteEndUser(callCtx, t.BusinessLine, t.EndUser)
			if err == nil && next.KernelRequestID == "" {
				err = task.ErrInvalid
			}
			if err == nil {
				next.Stage = "kernel_wait"
			}
		case "kernel_wait":
			if w.Kernel == nil {
				return w.retry(ctx, &out, task.ErrConfiguration)
			}
			var progress kernel.EndUserDeletion
			progress, err = w.Kernel.EndUserDeletion(callCtx, t.BusinessLine, next.KernelRequestID)
			if err == nil && progress.Done {
				next.Stage = "application_cleanup"
			}
		case "application_cleanup":
			// 账号、身份解绑、任务和全部应用凭据在一个事务清除；不留下可再次执行的旧任务。
			err = w.Accounts.Store.UpdateAccount(ctx, t.Owner, func(r *Record) error {
				r.Cleanup = true
				r.Account.Status = Deleted
				r.Account.UpdatedAt = w.Accounts.now()
				r.Account.TimeZone = ""
				r.Account.Preferences = nil
				r.Account.Entitlement = ""
				r.Account.EntitlementVersion = 0
				r.Account.SyncedVersion = 0
				r.Account.EffectiveTier = kernel.Tier{}
				r.Devices = map[string]Device{}
				r.Sessions = map[string]Session{}
				r.Deletion.Stage = "completed"
				r.Deletion.CompletedAt = w.Accounts.now()
				return nil
			})
			if err != nil {
				return w.retry(ctx, &out, err)
			}
			out.State = task.Succeeded
			return nil // 当前任务已随清理移除，不能重新 Save 复活它。
		default:
			err = task.ErrInvalid
		}
		if err != nil {
			return w.retry(ctx, &out, err)
		}
		if next != r.Deletion {
			err = w.Accounts.Store.UpdateAccount(ctx, t.Owner, func(r *Record) error { r.Deletion = next; return nil })
			if err != nil {
				return w.retry(ctx, &out, err)
			}
			out.RetryAt = w.Accounts.now()
		} else {
			out.RetryAt = w.Accounts.now().Add(task.Backoff(t.TaskID, 1, w.RetryBase, w.RetryMax))
		}
		out.LastErrorClass = ""
		return w.Tasks.Save(ctx, out, w.Accounts.now(), true)
	})
	return out, err
}

func (w *Worker) retry(ctx context.Context, t *task.Task, cause error) error {
	t.LastErrorClass = "account_retry"
	// 档位配置错误只记一次失败；注销不能因为外部故障成为假完成或不可恢复终态。
	if t.Operation == TierOperation && task.IsConfiguration(cause) {
		t.State = task.Failed
		t.LastErrorClass = "configuration"
	}
	t.RetryAt = w.Accounts.now().Add(task.Backoff(t.TaskID, t.Attempt, w.RetryBase, w.RetryMax))
	return errors.Join(cause, w.Tasks.Save(ctx, *t, w.Accounts.now(), true))
}
