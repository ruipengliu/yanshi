package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"xinli/kernel"
	"xinli/task"
)

// HTTP 替身保存完整日志且完全不对重复 input_id 去重，使测试不能误靠内核窗口通过。
type backend struct {
	mu          sync.Mutex
	events      []map[string]any
	posts       int
	reads       int
	lost        bool
	readFailure bool
	missing     bool
	reject      int
	complete    bool
	entered     chan struct{}
	release     <-chan struct{}
}

func (b *backend) append(kind, inputID, runID string) {
	payload := map[string]string{"run_id": runID}
	if inputID != "" {
		payload["input_id"] = inputID
	}
	n := len(b.events) + 1
	b.events = append(b.events, map[string]any{"seq": strconv.Itoa(n), "id": fmt.Sprintf("event-%d", n), "session_id": "session", kind: payload})
}

func (b *backend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && b.entered != nil {
		select {
		case b.entered <- struct{}{}:
		default:
		}
		select {
		case <-b.release:
		case <-r.Context().Done():
			return
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.missing {
		w.WriteHeader(404)
		return
	}
	if r.Method == http.MethodPost {
		b.posts++
		var in struct {
			InputID string `json:"input_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			w.WriteHeader(400)
			return
		}
		if b.reject != 0 {
			w.WriteHeader(b.reject)
			return
		}
		run := fmt.Sprintf("run-%d", b.posts)
		b.append("run_requested", in.InputID, run)
		if b.complete {
			b.append("run_completed", "", run)
		}
		if b.lost {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(map[string]any{"run_id": run, "steered": false})
		return
	}
	b.reads++
	if b.readFailure {
		w.WriteHeader(503)
		return
	}
	after, err := strconv.Atoi(r.URL.Query().Get("after"))
	if err != nil || after > len(b.events) {
		w.WriteHeader(400)
		return
	}
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		w.WriteHeader(400)
		return
	}
	page := b.events[after:min(after+limit, len(b.events))]
	if page == nil {
		page = []map[string]any{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"events": page})
}

func workerTests(t *testing.T, factory Factory) {
	ctx := context.Background()
	for _, mode := range []string{"lost_response", "beyond_64", "read_failure", "target_deleted", "absent_then_resend", "crash_before_send", "configuration"} {
		t.Run(mode, func(t *testing.T) {
			s, reopen := factory(t)
			id := enqueue(t, s, source(1), task.Spec{Operation: "collect", Target: "p"})
			b := &backend{lost: true, complete: true}
			if mode == "absent_then_resend" {
				b.reject = 503
			}
			if mode == "configuration" {
				b.reject = 403
			}
			httpServer := httptest.NewServer(b)
			defer httpServer.Close()
			now := epoch
			makeWorker := func(s task.Store) *task.Executor {
				return &task.Executor{Store: s, Kernel: &kernel.Client{BaseURL: httpServer.URL}, Now: func() time.Time { return now }, RetryBase: time.Second, RetryMax: time.Minute,
					Prepare: func(context.Context, task.Task) (string, string, error) { return "session", "整理今天", nil }}
			}
			w := makeWorker(s)
			if mode == "crash_before_send" {
				v := claim(t, s, id, now)
				v.State = task.Unknown
				v.Session = "session"
				if err := s.Save(ctx, v, now, false); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := w.Run(ctx, id); err == nil {
					t.Fatal("expected submit error")
				}
				want := task.Unknown
				if mode == "configuration" {
					want = task.Failed
				}
				if v := get(t, s, id); v.State != want {
					t.Fatalf("state=%s", v.State)
				}
			}
			b.mu.Lock()
			b.lost = false
			b.reject = 0
			if mode == "beyond_64" {
				for i := range 100 {
					b.append("run_requested", fmt.Sprintf("other-%d", i), fmt.Sprintf("other-run-%d", i))
				}
			}
			if mode == "read_failure" {
				b.readFailure = true
			}
			if mode == "target_deleted" {
				b.missing = true
			}
			b.mu.Unlock()
			// 重建执行器，PG 还关闭并重开连接池；恢复只依赖已持久化的任务与日志。
			s = reopen()
			w = makeWorker(s)
			now = epoch.Add(2 * time.Minute)
			_, err := w.Run(ctx, id)
			if mode == "read_failure" {
				if err == nil || get(t, s, id).State != task.Unknown {
					t.Fatal("read failure treated as absent", err)
				}
				b.mu.Lock()
				posts := b.posts
				b.readFailure = false
				b.mu.Unlock()
				if posts != 1 {
					t.Fatal("blind resend", posts)
				}
				now = now.Add(2 * time.Minute)
				if _, err := w.Run(ctx, id); err != nil {
					t.Fatal(err)
				}
			} else if mode == "target_deleted" {
				if !errors.Is(err, kernel.ErrNotFound) || get(t, s, id).State != task.Terminated {
					t.Fatal("deleted session retried", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			// 明确提交响应不含 Run 终态；下一次扫描再通过完整事件日志确认。
			if mode == "absent_then_resend" || mode == "crash_before_send" {
				now = now.Add(2 * time.Minute)
				if _, err := w.Run(ctx, id); err != nil {
					t.Fatal(err)
				}
			}
			v := get(t, s, id)
			if mode != "target_deleted" && mode != "configuration" && (v.State != task.Succeeded || v.Receipt.InputID != v.InputID || v.Receipt.Sequence == 0) {
				t.Fatal("receipt not recovered", v.State, v.Receipt)
			}
			b.mu.Lock()
			posts, reads := b.posts, b.reads
			b.mu.Unlock()
			wantPosts := 1
			if mode == "absent_then_resend" {
				wantPosts = 2
			}
			if posts != wantPosts {
				t.Fatal("duplicate send", posts, wantPosts)
			}
			if mode == "beyond_64" && reads < 5 {
				t.Fatal("log not fully paged", reads)
			}
			if mode == "configuration" {
				if reads != 0 {
					t.Fatal("configuration failure retried")
				}
				if n, err := s.FailureCount(ctx, source(1).Owner); n != 1 || err != nil {
					t.Fatal(n, err)
				}
			}
		})
	}
	t.Run("two_workers_one_submission", func(t *testing.T) {
		s, _ := factory(t)
		id := enqueue(t, s, source(1), task.Spec{Operation: "collect", Target: "p"})
		release := make(chan struct{})
		entered := make(chan struct{}, 1)
		b := &backend{entered: entered, release: release}
		server := httptest.NewServer(b)
		defer server.Close()
		var once sync.Once
		defer once.Do(func() { close(release) })
		w := &task.Executor{Store: s, Kernel: &kernel.Client{BaseURL: server.URL}, Now: func() time.Time { return epoch }, Prepare: func(context.Context, task.Task) (string, string, error) { return "session", "整理今天", nil }}
		done := make(chan error, 1)
		go func() { _, err := w.Run(ctx, id); done <- err }()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("first worker did not submit")
		}
		other := *w
		if _, err := other.Run(ctx, id); err != nil {
			t.Fatal(err)
		}
		once.Do(func() { close(release) })
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.posts != 1 || get(t, s, id).State != task.Accepted {
			t.Fatal("competing submissions", b.posts)
		}
	})
	t.Run("accepted_holds_next_command_until_terminal", func(t *testing.T) {
		s, _ := factory(t)
		first := enqueue(t, s, source(1), task.Spec{Operation: "collect", Target: "p"})
		second := enqueue(t, s, source(2), task.Spec{Operation: "collect", Target: "p"})
		b := &backend{}
		server := httptest.NewServer(b)
		defer server.Close()
		now := epoch
		w := &task.Executor{Store: s, Kernel: &kernel.Client{BaseURL: server.URL}, Now: func() time.Time { return now }, Prepare: func(context.Context, task.Task) (string, string, error) { return "session", "整理今天", nil }}
		if _, err := w.Run(ctx, first); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Hour)
		if _, err := w.Run(ctx, second); err != nil {
			t.Fatal(err)
		}
		b.mu.Lock()
		posts := b.posts
		b.append("run_completed", "", "run-1")
		b.mu.Unlock()
		if posts != 1 {
			t.Fatal("second command entered before terminal")
		}
		if _, err := w.Run(ctx, first); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Run(ctx, second); err != nil {
			t.Fatal(err)
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.posts != 2 {
			t.Fatal("second command not released", b.posts)
		}
	})
}
