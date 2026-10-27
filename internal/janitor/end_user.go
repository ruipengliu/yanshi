package janitor

import (
	"context"
	"errors"
	"time"

	"yanshi/internal/lifecycle"
)

func (j *Janitor) sweepEndUsers(ctx context.Context, now time.Time) error {
	barriers, err := j.Deletions.PendingEndUsers(ctx, 100)
	if err != nil {
		return err
	}
	grace := j.EndUserGrace
	if grace <= 0 {
		grace = lifecycle.DefaultEndUserGrace
	}
	for _, b := range barriers {
		// 写入方可能已把索引落盘，却没能把该 ID 加进屏障；按 EndUser 再找一遍可恢复这一步。
		ids, err := j.Index.IDsOf(ctx, b.BusinessLine, b.EndUser)
		if err != nil {
			return err
		}
		if err := j.Deletions.AddEndUserSessions(ctx, b, ids); err != nil {
			return err
		}
		b, err = j.Deletions.GetEndUser(ctx, b.BusinessLine, b.EndUser)
		if errors.Is(err, lifecycle.ErrNotFound) {
			continue // 其他 Janitor 已完成并清除了到期屏障
		}
		if err != nil {
			return err
		}
		complete := true
		for _, sid := range b.Sessions {
			t, err := j.Deletions.Get(ctx, sid)
			if errors.Is(err, lifecycle.ErrNotFound) {
				// 登记 ID 后、写 Session 删除记录前失败：由原屏障补写，避免重试永远等待。
				if _, err := j.Deletions.Mark(ctx, &lifecycle.Tombstone{SessionID: sid, BusinessLine: b.BusinessLine,
					RequestID: b.RequestID, Reason: "account_deletion", RequestedAt: now}); err != nil {
					return err
				}
				complete = false
				break
			}
			if err != nil {
				return err
			}
			if t.CompletedAt.IsZero() {
				complete = false
				break
			}
		}
		if complete {
			if err := j.Deletions.CompleteEndUser(ctx, b, now, now.Add(grace)); err != nil {
				return err
			}
		}
	}
	return j.Deletions.PruneEndUsers(ctx, now, 100)
}
