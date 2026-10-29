package recovery

import (
	"context"
	"strconv"
	"strings"
)

type shardContext struct{}

// WithShard 只选择应用库中的恢复进度，不改变业务线授权或信封归属。
func WithShard(ctx context.Context, id int) context.Context {
	return context.WithValue(ctx, shardContext{}, id)
}
func Shard(ctx context.Context) int {
	if id, ok := ctx.Value(shardContext{}).(int); ok {
		return id
	}
	return -1
}
func ProgressKey(ctx context.Context, line string) string {
	return strconv.Itoa(len(line)) + ":" + line + ":" + strconv.Itoa(Shard(ctx))
}

// ownsSession 只用于跳过其他分片的恢复候选；读取权限仍由内核逐 ID 校验。
func ownsSession(id string, shard int, slots []int) bool {
	if shard < 0 {
		return true
	}
	parts := strings.Split(id, "_")
	if len(parts) < 5 || parts[0] != "ses" || parts[1] != "v1" {
		return shard == 0
	}
	slot, err := strconv.ParseInt(parts[2], 32, 16)
	if err != nil {
		return false
	}
	for _, s := range slots {
		if int(slot) == s {
			return true
		}
	}
	return false
}
