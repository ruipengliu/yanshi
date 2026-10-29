package task

import "context"

// Reader 只按 owner 查询，不复用执行器按全局任务 ID 读取的入口。
type Reader interface {
	List(context.Context, Owner, int64, int) ([]Task, error)
	Read(context.Context, Owner, string) (Task, error)
}
