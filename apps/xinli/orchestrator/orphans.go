package orchestrator

import (
	"context"
	"errors"

	"persona/domain"
	"persona/store"
	"xinli/kernel"
	"xinli/task"
)

type orphanKernel interface {
	Sessions(context.Context, string, string) ([]kernel.SessionInfo, error)
	EmptySession(context.Context, string) (bool, error)
	DeleteSession(context.Context, string, string) error
	Deletion(context.Context, string, string) (kernel.Deletion, error)
}

// ScanOrphans 由启动与周期任务扫描调用；默认十分钟宽限，且不清理尚未过期的创建租约。
func (o *Orchestrator) ScanOrphans(ctx context.Context) error {
	intents, err := o.tasks.Creations(ctx, o.now().Add(-o.orphanGrace), 256)
	if err != nil || len(intents) == 0 {
		return err
	}
	k, ok := o.kernel.(orphanKernel)
	if !ok {
		return task.ErrConfiguration
	}
	var result error
	for _, intent := range intents {
		if intent.LeaseUntil.After(o.now()) {
			continue
		}
		err := o.cleanCreation(ctx, k, intent)
		result = errors.Join(result, err, o.tasks.DeferCreation(ctx, intent.ID, o.now()))
	}
	return result
}

func (o *Orchestrator) cleanCreation(ctx context.Context, k orphanKernel, intent task.Creation) error {
	if intent.Session != "" {
		done, err := o.cleanOrphan(ctx, k, intent, intent.Session)
		if err == nil && done {
			return o.tasks.ForgetCreation(ctx, intent.ID)
		}
		return err
	}
	list, err := k.Sessions(ctx, intent.BusinessLine, intent.EndUser)
	if err != nil {
		return err
	}
	for _, candidate := range list {
		if candidate.Agent != intent.Agent || candidate.CreatedAt.After(o.now().Add(-o.orphanGrace)) {
			continue
		}
		if _, err := o.cleanOrphan(ctx, k, intent, candidate.ID); err != nil {
			return err
		}
	}
	// 无回包的创建没有可证明的最终结束时刻。保留无正文扫描凭据，防止迟到提交产生永久孤儿。
	return nil
}

func (o *Orchestrator) cleanOrphan(ctx context.Context, k orphanKernel, intent task.Creation, sid string) (bool, error) {
	fenced := false
	err := o.store.Update(ctx, intent.BusinessLine, intent.EndUser, func(b *domain.Book) error {
		fenced = b.FenceOrphan(sid)
		return nil
	})
	if errors.Is(err, store.ErrOwnerDeleting) {
		fenced, err = true, nil
	}
	if err != nil {
		return false, err
	}
	if !fenced {
		return true, nil
	}
	empty, err := k.EmptySession(ctx, sid)
	if errors.Is(err, kernel.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !empty {
		return false, errors.New("orchestrator: orphan is not empty")
	}
	// 公开 DELETE 的默认服务删除类别适用于空孤儿；erasure 会保留 Memory，不用于这里。
	err = k.DeleteSession(ctx, sid, "")
	if err == nil || errors.Is(err, kernel.ErrNotFound) {
		return true, nil
	}
	if _, probeErr := k.Deletion(ctx, intent.BusinessLine, sid); probeErr == nil {
		return true, nil
	}
	return false, err
}
