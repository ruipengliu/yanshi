package task

import (
	"context"
	"fmt"
	"time"
)

// Creation 在调用内核之前落盘；无 Session 表示回包可能丢失，不能据此宣称没有孤儿。
type Creation struct {
	Owner
	ID         string
	TaskID     string
	Target     string
	Agent      string
	Generation int64
	Expected   string
	Version    uint64
	Revision   uint64
	DeleteAt   time.Time
	CheckedAt  time.Time
	CreatedAt  time.Time
	LeaseUntil time.Time
	Session    string
}

func CreationID(t Task) string {
	return fmt.Sprintf("%s/%d", t.TaskID, t.LeaseGeneration)
}

type CreationStore interface {
	BeginCreation(context.Context, Task, Creation, time.Time) error
	RecordCreated(context.Context, string, string) error
	Creations(context.Context, time.Time, int) ([]Creation, error)
	ForgetCreation(context.Context, string) error
	DeferCreation(context.Context, string, time.Time) error
}

func ValidCreation(t Task, c Creation, now time.Time) bool {
	return c.ID == CreationID(t) && c.Owner == t.Owner && c.TaskID == t.TaskID && c.Target == t.Target &&
		c.Generation == t.LeaseGeneration && c.LeaseUntil.Equal(t.LeaseUntil) && c.CreatedAt.Equal(now) &&
		c.Agent != "" && c.Session == "" && t.LeaseUntil.After(now)
}
