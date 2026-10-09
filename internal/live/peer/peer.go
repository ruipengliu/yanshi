// Package peer 让实时增量跨进程送达（docs/design/m2-live-deltas.md，ADR-0013）。
//
// 执行 Attempt 的进程把自己的内部地址写入 AttemptStarted.live_endpoint；
// 订阅方跟随 Session 日志找到当前 Attempt 的执行进程，直接从它的 Handler 拉取增量。
package peer

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/live"
)

// Path 是内部增量接口的路径前缀，其后是 Session ID。
const Path = "/internal/v1/live/"

const tokenHeader = "X-Yanshi-Peer-Token"

// pingInterval 是空闲时的保活间隔：让中间设备不因空闲断开连接，也让写失败尽早暴露。
const pingInterval = 15 * time.Second

// Handler 把本进程某个 Session 的增量以逐行 JSON 流式输出给其他进程。
// 只应挂在内部监听地址上；Token 非空时要求请求携带相同的令牌。
type Handler struct {
	Local live.Bus
	Token string
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get(tokenHeader)), []byte(h.Token)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	sid := strings.TrimPrefix(r.URL.Path, Path)
	if sid == "" || strings.Contains(sid, "/") {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	deltas, unsubscribe := h.Local.Subscribe(sid)
	defer unsubscribe()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	enc := json.NewEncoder(w)
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case d := <-deltas:
			if enc.Encode(d) != nil {
				return
			}
		case <-ping.C:
			if _, err := w.Write([]byte("\n")); err != nil {
				return
			}
		}
		flusher.Flush()
	}
}

// Bus 是跨进程的 live.Bus：发布只写本进程；订阅合并本进程的增量与当前 Attempt 执行进程的增量。
type Bus struct {
	Local *live.MemBus
	Log   eventlog.Log
	// Self 是本进程的内部地址（与 runtime.Config.LiveEndpoint 相同），指向自己的 Attempt 不需要远程拉取。
	Self   string
	Token  string
	Client *http.Client
	// Retry 是连接断开后的重连间隔，默认 1 秒。
	Retry  time.Duration
	Logger *slog.Logger
}

func (b *Bus) Publish(d live.Delta) { b.Local.Publish(d) }

func (b *Bus) Subscribe(sessionID string) (<-chan live.Delta, func()) {
	out := make(chan live.Delta, 256)
	local, unsubscribe := b.Local.Subscribe(sessionID)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case d := <-local:
				offer(out, d)
			}
		}
	}()
	go b.follow(ctx, sessionID, out)
	return out, func() { cancel(); unsubscribe() }
}

// offer 与 MemBus 一致：慢订阅者丢增量，而不是阻塞上游。
func offer(out chan<- live.Delta, d live.Delta) {
	select {
	case out <- d:
	default:
	}
}

// follow 跟随 Session 日志，始终从当前 running Attempt 的执行进程拉取增量；
// Attempt 切换时改连新进程，Run 挂起或终态时断开。
func (b *Bus) follow(ctx context.Context, sessionID string, out chan<- live.Delta) {
	var after uint64
	var endpoint, connected string
	stop := func() {}
	defer func() { stop() }()
	for {
		events, err := eventlog.ReadAll(ctx, b.Log, sessionID, after)
		if err != nil {
			if !b.sleep(ctx) {
				return
			}
			continue
		}
		for _, e := range events {
			after = e.GetSeq()
			switch p := e.GetPayload().(type) {
			case *v1.Event_AttemptStarted:
				endpoint = p.AttemptStarted.GetLiveEndpoint()
			case *v1.Event_RunSuspended, *v1.Event_RunCompleted, *v1.Event_RunFailed, *v1.Event_RunInterrupted:
				endpoint = ""
			}
		}
		want := endpoint
		if want == b.Self {
			want = "" // 本进程的增量已由 Local 送达
		}
		if want != connected {
			stop()
			stop, connected = func() {}, want
			if want != "" {
				pctx, cancel := context.WithCancel(ctx)
				go b.pull(pctx, want, sessionID, out)
				stop = cancel
			}
		}
		if _, err := b.Log.Wait(ctx, sessionID, after); err != nil && !b.sleep(ctx) {
			return
		}
	}
}

// pull 从 endpoint 持续拉取增量，断开后按 Retry 重连，直到 ctx 结束。
func (b *Bus) pull(ctx context.Context, endpoint, sessionID string, out chan<- live.Delta) {
	for {
		err := b.stream(ctx, endpoint, sessionID, out)
		if ctx.Err() != nil {
			return
		}
		if b.Logger != nil {
			b.Logger.Debug("live peer stream ended", "endpoint", endpoint, "session", sessionID, "err", err)
		}
		if !b.sleep(ctx) {
			return
		}
	}
}

func (b *Bus) stream(ctx context.Context, endpoint, sessionID string, out chan<- live.Delta) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+Path+url.PathEscape(sessionID), nil)
	if err != nil {
		return err
	}
	if b.Token != "" {
		req.Header.Set(tokenHeader, b.Token)
	}
	client := b.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("live peer %s: http %d", endpoint, resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue // 保活
		}
		var d live.Delta
		if err := json.Unmarshal(sc.Bytes(), &d); err != nil {
			return err
		}
		if d.SessionID == sessionID {
			offer(out, d)
		}
	}
	return sc.Err()
}

func (b *Bus) sleep(ctx context.Context) bool {
	d := b.Retry
	if d == 0 {
		d = time.Second
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
