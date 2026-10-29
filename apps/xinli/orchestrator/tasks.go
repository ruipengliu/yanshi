package orchestrator

import (
	"context"
	"errors"
	"strings"
	"time"

	"persona/domain"
	"persona/store"
	"xinli/task"
)

var errRebuildPreparation = errors.New("orchestrator: rebuild preparation failed")

// StartTasks 启动立即扫描；通知只是加速路径，重启不依赖通知重新投递。
func (o *Orchestrator) StartTasks(tick <-chan time.Time) {
	o.mu.Lock()
	if o.closed || o.startedTasks {
		o.mu.Unlock()
		return
	}
	o.startedTasks = true
	o.pending.Add(1)
	o.mu.Unlock()
	go func() {
		defer o.pending.Done()
		if tick == nil {
			timer := time.NewTicker(o.retryInterval)
			defer timer.Stop()
			tick = timer.C
		}
		for {
			if err := o.ScanTasks(o.ctx); err != nil {
				o.failed("task_scan", "", "", "", err)
			}
			select {
			case <-o.stopRetries:
				return
			case <-o.ctx.Done():
				return
			case _, ok := <-tick:
				if !ok {
					return
				}
			}
		}
	}()
}

func (o *Orchestrator) ScanTasks(ctx context.Context) error {
	if !o.begin() {
		return errors.New("orchestrator: shutting down")
	}
	defer o.pending.Done()
	owners, err := o.store.PendingChangeOwners(ctx)
	result := err
	for _, owner := range owners {
		result = errors.Join(result, o.receiveChanges(ctx, owner.BusinessLine, owner.EndUser))
	}
	return errors.Join(result, o.scanInputs(ctx), o.ScanOrphans(ctx))
}

func (o *Orchestrator) scanInputs(ctx context.Context) error {
	ids, err := o.tasks.Due(ctx, o.now(), 256)
	if err != nil {
		return err
	}
	var result error
	for _, id := range ids {
		t, err := o.tasks.Get(ctx, id)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		// 删除步骤仍由 Persona 的扫描顺序驱动；输入任务由同一 receipt 执行器处理。
		if t.Operation == "delete" {
			result = errors.Join(result, o.resumeDeletion(ctx, t))
			continue
		}
		if t.Operation == "ensure" {
			job, claimed, claimErr := o.tasks.Claim(ctx, id, o.now(), 2*time.Minute)
			if claimed {
				_, claimErr = o.runEnsure(ctx, job)
			}
			result = errors.Join(result, claimErr)
			continue
		}
		_, err = o.worker.Run(ctx, id)
		result = errors.Join(result, err)
	}
	return result
}

func (o *Orchestrator) resumeDeletion(ctx context.Context, t task.Task) error {
	err := o.processDeletions(ctx, t.BusinessLine, t.EndUser)
	if err == nil {
		return nil
	}
	// 读 Persona 失败发生在原扫描认领之前，也必须退避；注销 owner 不能永久占据发现队列。
	job, claimed, claimErr := o.tasks.Claim(ctx, t.TaskID, o.now(), 2*time.Minute)
	if claimErr != nil || !claimed {
		return errors.Join(err, claimErr)
	}
	job.LastErrorClass = "deletion_read"
	job.RetryAt = o.now().Add(task.Backoff(job.TaskID, job.Attempt, o.retryInterval, o.retryMax))
	if errors.Is(err, store.ErrOwnerDeleting) {
		job.State = task.Terminated
		job.LastErrorClass = "target_deleted"
	}
	return errors.Join(err, o.tasks.Save(ctx, job, o.now(), true))
}

func (o *Orchestrator) receiveChanges(ctx context.Context, bl, eu string) error {
	changes, err := o.store.PendingDomainChanges(ctx, bl, eu)
	if err != nil {
		return err
	}
	for _, change := range changes {
		// 周回顾的接受映射在 A10 接入；不能提前确认尚未接收的变化。
		if change.Kind != domain.FactDay {
			continue
		}
		specs, err := o.dayTasks(ctx, change)
		if err != nil {
			return err
		}
		source := task.Source{Owner: task.Owner{BusinessLine: bl, EndUser: eu}, Kind: change.Kind, ID: change.ID, Version: change.Version}
		if err := o.tasks.Receive(ctx, source, specs, o.now()); err != nil {
			return err
		}
		if err := o.store.AckDomainChanges(ctx, bl, eu, change.ID, change.Version, o.now()); err != nil {
			return err
		}
	}
	return nil
}

func (o *Orchestrator) dayTasks(ctx context.Context, change store.DomainChange) ([]task.Spec, error) {
	var specs []task.Spec
	err := o.store.View(ctx, change.BusinessLine, change.EndUser, func(b *domain.Book) error {
		// 已擦除的事实仍须持久接收空映射，然后才能确认；不借旧变化复活被擦除的工作。
		if _, active := b.AcceptedFactByID(change.ID); !active {
			return nil
		}
		for _, id := range b.DayProjects(change.ID) {
			specs = append(specs, task.Spec{Operation: "collect", Target: "project/" + id, Date: change.Date})
		}
		return nil
	})
	return specs, err
}

func (o *Orchestrator) prepareTask(ctx context.Context, t task.Task) (sessionID, command string, err error) {
	defer func() {
		if errors.Is(err, store.ErrOwnerDeleting) || errors.Is(err, domain.ErrDayDeleted) || errors.Is(err, domain.ErrCapturesExpired) || errors.Is(err, domain.ErrWorkTargetGone) {
			err = errors.Join(task.ErrObsolete, err)
		}
	}()
	switch t.Operation {
	case "collect":
		return o.prepareCollect(ctx, t)
	case "rebuild":
		return o.prepareRebuild(ctx, t)
	case "day", "exchange":
		sid, err := o.ensureWork(ctx, t, DayAgent)
		text := "整理今天"
		if t.Operation == "exchange" {
			text = "整理交谈"
		}
		return sid, text, err
	case "week":
		sid, err := o.ensureWork(ctx, t, WeekAgent)
		return sid, "回顾本周", err
	case "rewrite":
		sid, err := o.ensureWork(ctx, t, ProjectAgent)
		return sid, "重写现状", err
	default:
		return "", "", task.ErrConfiguration
	}
}

// 旧 Week 开发入口也经过 Session 槽位，避免绕过归集的单在途限制。
// 触发重写的时机仍保留旧行为，A10 再迁到周回顾接受变化。
func (o *Orchestrator) weekInput(ctx context.Context, bl, eu, from, operation, target string) (task.Task, error) {
	source := task.Source{Owner: task.Owner{BusinessLine: bl, EndUser: eu}, Kind: "week_request", ID: "week_request/" + from + "/" + operation + "/" + target, Version: 1}
	spec := task.Spec{Operation: operation, Target: target, Date: from}
	if err := o.tasks.Receive(ctx, source, []task.Spec{spec}, o.now()); err != nil {
		return task.Task{}, err
	}
	return o.worker.Run(ctx, task.Identity(source, spec))
}

func (o *Orchestrator) prepareCollect(ctx context.Context, t task.Task) (string, string, error) {
	id := strings.TrimPrefix(t.Target, "project/")
	var found bool
	var sid string
	err := o.store.View(ctx, t.BusinessLine, t.EndUser, func(b *domain.Book) error {
		if _, active := b.AcceptedFactByID(t.ID); !active {
			return task.ErrObsolete
		}
		for _, p := range b.State().Projects {
			if p.ID == id {
				found = true
				sid = p.Session
				break
			}
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	if !found {
		return "", "", task.ErrObsolete
	}
	sid, err = o.ensureWork(ctx, t, ProjectAgent)
	if err != nil {
		return "", "", err
	}
	return sid, "整理 " + t.Date, nil
}

func (o *Orchestrator) prepareRebuild(ctx context.Context, t task.Task) (string, string, error) {
	var deletion domain.DeletionTask
	err := o.store.View(ctx, t.BusinessLine, t.EndUser, func(b *domain.Book) error {
		for _, d := range b.DeletionTasks() {
			if "rebuild/"+d.ID == t.ID {
				deletion = d
				break
			}
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	if deletion.ID == "" || !deletion.RebuildPending {
		return "", "", task.ErrObsolete
	}
	skip, err := o.skipRebuild(ctx, t.BusinessLine, t.EndUser, deletion)
	if err != nil {
		return "", "", err
	}
	if skip {
		return "", "", task.ErrObsolete
	}
	sid, err := o.ensureWork(ctx, t, DayAgent)
	if err != nil {
		return "", "", errors.Join(errRebuildPreparation, err)
	}
	return sid, "整理今天", err
}

func deletionSource(bl, eu, id, kind string) task.Source {
	return task.Source{Owner: task.Owner{BusinessLine: bl, EndUser: eu}, Kind: "deletion", ID: kind + "/" + id, Version: 1}
}

func (o *Orchestrator) rebuildTask(ctx context.Context, bl, eu string, d domain.DeletionTask) (task.Task, error) {
	source := deletionSource(bl, eu, d.ID, "rebuild")
	spec := task.Spec{Operation: "rebuild", Target: "day/" + d.Date, Date: d.Date}
	if err := o.tasks.Receive(ctx, source, []task.Spec{spec}, o.now()); err != nil {
		return task.Task{}, err
	}
	return o.worker.Run(ctx, task.Identity(source, spec))
}
