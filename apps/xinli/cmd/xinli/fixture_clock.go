package main

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// fixtureClock 只接独立评测进程；复制父进程的偏移而非采样时间，避免进程间传输耗时造成时钟漂移。
type fixtureClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *fixtureClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *fixtureClock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Offset *string `json:"offset"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.Offset == nil {
		http.Error(w, "invalid clock offset", http.StatusBadRequest)
		return
	}
	var trailing any
	offset, err := time.ParseDuration(*in.Offset)
	if err != nil || dec.Decode(&trailing) != io.EOF {
		http.Error(w, "invalid clock offset", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// 初始偏移可以为负；运行之后只接受相同或更大的偏移，保证在途租约与过期判断不倒退。
	if offset < c.offset {
		http.Error(w, "clock cannot go backwards", http.StatusBadRequest)
		return
	}
	c.offset = offset
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, "{}")
}
