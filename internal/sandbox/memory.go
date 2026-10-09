package sandbox

import (
	"bytes"
	"context"
	"io"
	"sort"
	"sync"
	"time"
)

// MemActivity 是 Activity 的进程内实现。
type MemActivity struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func NewMemActivity() *MemActivity { return &MemActivity{last: map[string]time.Time{}} }

func (a *MemActivity) Touch(_ context.Context, id string, at time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if at.After(a.last[id]) {
		a.last[id] = at
	}
	return nil
}

func (a *MemActivity) ClaimIdle(_ context.Context, before time.Time, limit int) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for id, t := range a.last {
		if t.Before(before) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	for _, id := range out {
		delete(a.last, id)
	}
	return out, nil
}

// Fake 是内存中的 Provider，用于测试与确定性模拟。工作区文件在 Stop 后保留。
// ExecFunc 决定 Exec 的行为；为 nil 时返回 stdout "ok"。
type Fake struct {
	ExecFunc func(ctx context.Context, id string, req ExecRequest) (*ExecResult, error)

	mu      sync.Mutex
	running map[string]bool
	files   map[string]map[string][]byte
	// Ensures 统计创建或恢复沙箱的次数。
	Ensures int
}

func NewFake() *Fake {
	return &Fake{running: map[string]bool{}, files: map[string]map[string][]byte{}}
}

func (f *Fake) Ensure(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running[id] {
		f.running[id] = true
		f.Ensures++
	}
	if f.files[id] == nil {
		f.files[id] = map[string][]byte{}
	}
	return nil
}

func (f *Fake) Running(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[id]
}

func (f *Fake) Exec(ctx context.Context, id string, req ExecRequest) (*ExecResult, error) {
	if !f.Running(id) {
		return nil, errNotRunning(id)
	}
	if f.ExecFunc != nil {
		return f.ExecFunc(ctx, id, req)
	}
	return &ExecResult{Stdout: []byte("ok")}, nil
}

func (f *Fake) CopyIn(_ context.Context, id, p string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running[id] {
		return errNotRunning(id)
	}
	f.files[id][p] = data
	return nil
}

func (f *Fake) CopyOut(_ context.Context, id, p string, w io.Writer) error {
	f.mu.Lock()
	b, ok := f.files[id][p]
	running := f.running[id]
	f.mu.Unlock()
	if !running {
		return errNotRunning(id)
	}
	if !ok {
		return errNoFile(p)
	}
	_, err := io.Copy(w, bytes.NewReader(b))
	return err
}

func (f *Fake) ReadFile(_ context.Context, id, p string, max int) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running[id] {
		return nil, false, errNotRunning(id)
	}
	b, ok := f.files[id][p]
	if !ok {
		return nil, false, errNoFile(p)
	}
	if len(b) > max {
		return b[:max], true, nil
	}
	return b, false, nil
}

func (f *Fake) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.running, id)
	return nil
}

func (f *Fake) Destroy(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.running, id)
	delete(f.files, id)
	return nil
}

type sandboxError string

func (e sandboxError) Error() string { return string(e) }

func errNotRunning(id string) error { return sandboxError("sandbox " + id + " is not running") }
func errNoFile(p string) error      { return sandboxError("no such file: " + p) }

func (a *MemActivity) Delete(_ context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.last, id)
	return nil
}

// Exists 报告沙箱的工作区是否仍存在（Destroy 之后为 false）。
func (f *Fake) Exists(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files[id] != nil
}
