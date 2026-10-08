// Package ids 生成全局唯一 ID。组件通过注入 Generator 取 ID，以便模拟测试可复现。
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
)

type Generator func() string

// Random 返回 128 位随机 ID 的生成器。
func Random() Generator {
	return func() string {
		var b [16]byte
		_, _ = rand.Read(b[:])
		return hex.EncodeToString(b[:])
	}
}

// Sequential 返回形如 prefix-1, prefix-2 的确定性生成器，用于测试。
func Sequential(prefix string) Generator {
	var n atomic.Uint64
	return func() string { return fmt.Sprintf("%s-%d", prefix, n.Add(1)) }
}
