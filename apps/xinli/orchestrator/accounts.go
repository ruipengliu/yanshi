package orchestrator

import (
	"context"
	"persona/store"
	"xinli/task"
)

func (o *Orchestrator) withActive(ctx context.Context, owner task.Owner, fn func(context.Context) error) error {
	if o.accounts == nil {
		return fn(ctx)
	}
	return o.accounts.WithActive(ctx, owner, fn)
}
func (o *Orchestrator) runTask(ctx context.Context, id string) (task.Task, error) {
	t, err := o.tasks.Get(ctx, id)
	if err != nil {
		return t, err
	}
	var out task.Task
	err = o.withActive(ctx, t.Owner, func(ctx context.Context) error { var err error; out, err = o.worker.Run(ctx, id); return err })
	return out, err
}
func (o *Orchestrator) receiveChanges(ctx context.Context, bl, eu string) error {
	return o.withActive(ctx, task.Owner{BusinessLine: bl, EndUser: eu}, func(ctx context.Context) error { return o.receiveChangesActive(ctx, bl, eu) })
}

func (o *Orchestrator) scheduleOwner(ctx context.Context, owner store.Owner) error {
	return o.withActive(ctx, task.Owner{BusinessLine: owner.BusinessLine, EndUser: owner.EndUser}, func(ctx context.Context) error { return o.scheduleOwnerActive(ctx, owner) })
}

func (o *Orchestrator) DayInput(ctx context.Context, bl, eu, date, kind, inputID string) (string, error) {
	var out string
	err := o.withActive(ctx, task.Owner{BusinessLine: bl, EndUser: eu}, func(ctx context.Context) error {
		var err error
		out, err = o.dayInputActive(ctx, bl, eu, date, kind, inputID)
		return err
	})
	return out, err
}

func (o *Orchestrator) ensureDay(ctx context.Context, bl, eu, date string) (string, error) {
	var out string
	err := o.withActive(ctx, task.Owner{BusinessLine: bl, EndUser: eu}, func(ctx context.Context) error {
		var err error
		out, err = o.ensureDayActive(ctx, bl, eu, date)
		return err
	})
	return out, err
}

func (o *Orchestrator) runEnsure(ctx context.Context, job task.Task) (string, error) {
	var out string
	err := o.withActive(ctx, job.Owner, func(ctx context.Context) error { var err error; out, err = o.runEnsureActive(ctx, job); return err })
	return out, err
}

func (o *Orchestrator) Week(ctx context.Context, bl, eu string) (string, error) {
	var out string
	err := o.withActive(ctx, task.Owner{BusinessLine: bl, EndUser: eu}, func(ctx context.Context) error { var err error; out, err = o.weekActive(ctx, bl, eu); return err })
	return out, err
}
