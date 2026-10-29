package shard

import (
	"errors"
	"fmt"
)

var ErrUnavailable = errors.New("shard_unavailable")

// Unavailable 不携带 SQL、DSN 或用户数据，调用方可安全返回可重试错误。
func Unavailable(id int) error { return fmt.Errorf("shard %d: %w", id, ErrUnavailable) }
