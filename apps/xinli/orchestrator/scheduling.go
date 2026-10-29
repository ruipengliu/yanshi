package orchestrator

import (
	"context"
	"errors"
	"strings"
	"time"

	"persona/domain"
	"persona/store"
	"xinli/schedule"
	"xinli/task"
)

// StartSchedules 与任务恢复分开开关；评测只手动提交，不启动这个循环。
func (o *Orchestrator) StartSchedules(tick <-chan time.Time) {
	o.mu.Lock()
	if o.closed || o.startedSchedules {
		o.mu.Unlock()
		return
	}
	o.startedSchedules = true
	o.pending.Add(1)
	o.mu.Unlock()
	go func() {
		defer o.pending.Done()
		if tick == nil {
			timer := time.NewTicker(schedule.ScanInterval)
			defer timer.Stop()
			tick = timer.C
		}
		for {
			if err := o.ScanSchedules(o.ctx); err != nil {
				o.failed("schedule_scan", "", "", "", err)
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

// 扫描只持久接收计划；网络提交由 A8 执行器认领，重启不会丢失退避与失败状态。
func (o *Orchestrator) ScanSchedules(ctx context.Context) error {
	if !o.begin() {
		return errors.New("orchestrator: shutting down")
	}
	defer o.pending.Done()
	owners, err := o.store.SchedulingOwners(ctx)
	if err != nil {
		return err
	}
	var result error
	for _, owner := range owners {
		err := o.scheduleOwner(ctx, owner)
		if !errors.Is(err, store.ErrOwnerDeleting) {
			result = errors.Join(result, err)
		}
	}
	return errors.Join(result, o.scanInputs(ctx))
}

type scheduledInput struct {
	source task.Source
	spec   task.Spec
}

func scheduled(owner task.Owner, kind, date string, version uint64) scheduledInput {
	operation, target := "day", "day/"+date
	if kind == "week" {
		operation, target = "week", "week/"+date
	}
	return scheduledInput{task.Source{Owner: owner, Kind: "schedule_" + kind, ID: "schedule/" + kind + "/" + date, Version: version},
		task.Spec{Operation: operation, Target: target, Date: date}}
}

func (o *Orchestrator) scheduleOwnerActive(ctx context.Context, owner store.Owner) error {
	now := o.now()
	key := task.Owner{BusinessLine: owner.BusinessLine, EndUser: owner.EndUser}
	var inputs []scheduledInput
	err := o.store.Update(ctx, owner.BusinessLine, owner.EndUser, func(b *domain.Book) error {
		b.SetCaptureClock(o.now)
		if err := b.PrepareScheduling(); err != nil {
			return err
		}
		days := b.SchedulingDays()
		for _, day := range days {
			z, err := time.LoadLocation(day.TimeZone)
			if err != nil {
				return err
			}
			if day.Deleted || day.Revision == 0 {
				continue
			}
			daytime, final, err := schedule.DayTimes(day.Date, z, day.LastBatchAt, schedule.Jitter(owner.BusinessLine, owner.EndUser, day.Date))
			if err != nil {
				return err
			}
			kind := "day"
			if !now.Before(final) {
				kind = "final"
			} else if now.Before(daytime) || now.In(z).Format(time.DateOnly) != day.Date {
				continue
			}
			fact, accepted := b.AcceptedFact(domain.FactDay, day.Date)
			if accepted && day.Processed && !fact.AcceptedAt.Before(day.LastBatchAt) && (kind == "day" || !fact.AcceptedAt.Before(final)) {
				continue
			}
			inputs = append(inputs, scheduled(key, kind, day.Date, day.Revision))
		}
		// 候选向前留一天覆盖 UTC 两侧时区，再按该周固定时区向后找已到点的周。
		from, due := schedule.Week(now.Add(24*time.Hour), time.UTC, owner.BusinessLine, owner.EndUser)
		zone := o.defaultZone
		for {
			start, _ := time.Parse(time.DateOnly, from)
			end := start.AddDate(0, 0, 6).Format(time.DateOnly)
			weekZone := o.defaultZone
			for _, day := range days {
				if day.Date > end {
					break
				}
				weekZone, _ = time.LoadLocation(day.TimeZone)
				if day.Date >= from {
					break
				}
			}
			sunday, _ := time.ParseInLocation(time.DateOnly, end, weekZone)
			due = time.Date(sunday.Year(), sunday.Month(), sunday.Day(), 23, 0, 0, 0, weekZone).Add(schedule.Jitter(owner.BusinessLine, owner.EndUser, from))
			zone = weekZone
			if !due.After(now) {
				break
			}
			from = start.AddDate(0, 0, -7).Format(time.DateOnly)
		}
		// 新 owner 不补造启用前的周；历史业务日仍使停机跨周后的最新回顾可被发现。
		if len(days) > 0 && days[0].Date <= due.In(zone).Format(time.DateOnly) {
			if _, accepted := b.AcceptedFact(domain.FactWeek, from); !accepted {
				b.Archive(due)
				inputs = append(inputs, scheduled(key, "week", from, 1))
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, input := range inputs {
		if err := o.tasks.Receive(ctx, input.source, []task.Spec{input.spec}, now); err != nil {
			return err
		}
	}
	return nil
}

func (o *Orchestrator) completeScheduledTask(ctx context.Context, t task.Task) (bool, error) {
	if !strings.HasPrefix(t.Kind, "schedule_") {
		return true, nil
	}
	done := false
	err := o.store.View(ctx, t.BusinessLine, t.EndUser, func(b *domain.Book) error {
		kind := domain.FactDay
		if t.Operation == "week" {
			kind = domain.FactWeek
		}
		done = b.AcceptedCommand(kind, t.Date, t.InputID)
		if kind == domain.FactDay {
			done = false
			for _, day := range b.SchedulingDays() {
				if day.Date == t.Date {
					done = day.Processed && b.AcceptedCommand(kind, t.Date, t.InputID)
					break
				}
			}
		}
		return nil
	})
	return done, err
}

// 每次认领重查生命周期，包含等待配额和未知提交；期限到了不能继续排队延长原文生命。
func (o *Orchestrator) checkScheduledTask(ctx context.Context, t task.Task) error {
	if !strings.HasPrefix(t.Kind, "schedule_") {
		return nil
	}
	err := o.store.View(ctx, t.BusinessLine, t.EndUser, func(b *domain.Book) error {
		if t.Operation == "week" {
			return nil
		}
		for _, day := range b.SchedulingDays() {
			if day.Date != t.Date {
				continue
			}
			if day.Deleted {
				return task.ErrObsolete
			}
			if day.Processed && b.AcceptedCommand(domain.FactDay, t.Date, t.InputID) {
				return nil
			}
			if !o.now().Before(day.ExpiresAt) {
				return task.ErrExpired
			}
			// 新批次可以在整理期间到达；不能把追加误当擦除而打断正在推进的命令。
			if t.Session != "" {
				if err := b.CheckWorkSession(t.Session); err != nil {
					return task.ErrObsolete
				}
			}
			return nil
		}
		return task.ErrObsolete
	})
	if errors.Is(err, store.ErrOwnerDeleting) {
		return task.ErrObsolete
	}
	return err
}
