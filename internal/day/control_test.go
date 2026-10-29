package day

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type offsetClock struct{ offset atomic.Int64 }

func (c *offsetClock) Now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }
func (c *offsetClock) advance(t *testing.T, at time.Time) {
	t.Helper()
	next := int64(time.Until(at))
	if next < c.offset.Load() {
		t.Fatal("business clock cannot move backwards")
	}
	c.offset.Store(next)
}

type barrier struct {
	match   string
	seen    chan json.RawMessage
	release chan struct{}
	once    sync.Once
	reject  atomic.Bool
}

func (b *barrier) open() { b.once.Do(func() { close(b.release) }) }
func (b *barrier) wait(t *testing.T) json.RawMessage {
	t.Helper()
	select {
	case raw := <-b.seen:
		return raw
	case <-time.After(40 * time.Second):
		t.Fatal("fault boundary not reached")
	}
	return nil
}

type control struct {
	*offsetClock
	server      *httptest.Server
	token       string
	mu          sync.Mutex
	barriers    map[string]*barrier
	pushes      []map[string]string
	diagnostics map[string]json.RawMessage
}

func newControl(t *testing.T) *control {
	c := &control{offsetClock: &offsetClock{}, token: randomID(t), barriers: map[string]*barrier{}, diagnostics: map[string]json.RawMessage{}}
	c.advance(t, time.Date(2035, 1, 8, 1, 0, 0, 0, time.UTC)) // 周一当地 09:00。
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+c.token {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/clock" {
			_ = json.NewEncoder(w).Encode(map[string]int64{"offset": c.offset.Load()})
			return
		}
		if r.URL.Path != "/hook" {
			w.WriteHeader(404)
			return
		}
		var in struct {
			Point string          `json:"point"`
			Value json.RawMessage `json:"value"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in) != nil {
			w.WriteHeader(400)
			return
		}
		c.mu.Lock()
		if in.Point == "scheduler.discovery_started" || in.Point == "scheduler.discovery_completed" {
			c.diagnostics[in.Point] = append(json.RawMessage(nil), in.Value...)
		}
		if in.Point == "push.sent" {
			var ids map[string]string
			if json.Unmarshal(in.Value, &ids) == nil {
				c.pushes = append(c.pushes, ids)
			}
		}
		b := c.barriers[in.Point]
		c.mu.Unlock()
		if b != nil && (b.match == "" || strings.Contains(string(in.Value), b.match)) {
			select {
			case b.seen <- in.Value:
			default:
			}
			select {
			case <-b.release:
				if b.reject.Load() {
					w.WriteHeader(503)
					return
				}
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(func() {
		c.mu.Lock()
		for _, b := range c.barriers {
			b.open()
		}
		c.mu.Unlock()
		c.server.Close()
	})
	return c
}
func (c *control) arm(t *testing.T, point, match string) *barrier {
	b := &barrier{match: match, seen: make(chan json.RawMessage, 1), release: make(chan struct{})}
	c.mu.Lock()
	if old := c.barriers[point]; old != nil {
		old.open()
	}
	c.barriers[point] = b
	c.mu.Unlock()
	t.Cleanup(b.open)
	return b
}
func (c *control) sent(run, kind string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, p := range c.pushes {
		if p["run_id"] == run && p["kind"] == kind {
			n++
		}
	}
	return n
}

// 固定代理地址跨子进程重启保持不变；只在采集请求上丢弃已完成的响应。
type frontProxy struct {
	server  *httptest.Server
	mu      sync.RWMutex
	target  *httputil.ReverseProxy
	offline atomic.Bool
	lose    atomic.Bool
}

func newFront(t *testing.T) *frontProxy {
	p := &frontProxy{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.RLock()
		target := p.target
		p.mu.RUnlock()
		if p.offline.Load() || target == nil {
			w.WriteHeader(503)
			return
		}
		target.ServeHTTP(w, r)
	}))
	t.Cleanup(p.server.Close)
	return p
}
func (p *frontProxy) set(t *testing.T, address string) {
	u, err := url.Parse(address)
	must(t, err)
	target := httputil.NewSingleHostReverseProxy(u)
	target.ModifyResponse = func(r *http.Response) error {
		if r.Request.URL.Path == "/v1/captures/batches" && p.lose.CompareAndSwap(true, false) {
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	target.ErrorHandler = func(http.ResponseWriter, *http.Request, error) { panic(http.ErrAbortHandler) }
	p.mu.Lock()
	p.target = target
	p.mu.Unlock()
}

// 切断已经建立的连接，避免只关 listener 而连接池仍能访问数据库。
type tcpProxy struct {
	listener    net.Listener
	mu          sync.Mutex
	offline     bool
	connections map[net.Conn]bool
}

func newTCPProxy(t *testing.T, dsn string) (*tcpProxy, string) {
	cfg, err := pgx.ParseConfig(dsn)
	must(t, err)
	if strings.HasPrefix(cfg.Host, "/") {
		t.Fatal("TCP proxy requires a TCP PostgreSQL address")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	p := &tcpProxy{listener: listener, connections: map[net.Conn]bool{}}
	target := net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port)))
	go func() {
		for {
			in, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				p.mu.Lock()
				if p.offline {
					p.mu.Unlock()
					_ = in.Close()
					return
				}
				p.connections[in] = true
				p.mu.Unlock()
				defer func() { _ = in.Close(); p.mu.Lock(); delete(p.connections, in); p.mu.Unlock() }()
				out, err := net.DialTimeout("tcp", target, 2*time.Second)
				if err != nil {
					return
				}
				defer out.Close()
				done := make(chan struct{})
				go func() { _, _ = io.Copy(out, in); _ = out.Close(); close(done) }()
				_, _ = io.Copy(in, out)
				_ = in.Close()
				<-done
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); p.cut(true) })
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		must(t, err)
		u.Host = listener.Addr().String()
		q := u.Query()
		q.Del("host")
		q.Del("port")
		u.RawQuery = q.Encode()
		return p, u.String()
	}
	host, port, err := net.SplitHostPort(listener.Addr().String())
	must(t, err)
	return p, dsn + " host=" + host + " port=" + port
}
func (p *tcpProxy) cut(offline bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.offline = offline
	if offline {
		for conn := range p.connections {
			_ = conn.Close()
		}
	}
}

func eventually(t *testing.T, label string, fn func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for !fn() {
		select {
		case <-ctx.Done():
			t.Fatalf("timeout: %s", label)
		case <-time.After(30 * time.Millisecond):
		}
	}
}

func (c *control) disarm(point string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b := c.barriers[point]; b != nil {
		b.open()
	}
	delete(c.barriers, point)
}

func (c *control) sentID(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, p := range c.pushes {
		if p["id"] == id {
			n++
		}
	}
	return n
}
