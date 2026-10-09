package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/auth"
	"yanshi/sdk/nodesdk"
)

// benchCmd 是压测客户端（docs/design/m2-scale-test.md §2）：S 个 EndUser 各持有一个 Session 与 SSE 连接，
// 连续提交 Run，测量吞吐、端到端延迟（提交到收到终态事件）与首个增量延迟。
func benchCmd(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	servers := fs.String("servers", "http://127.0.0.1:8080", "API 地址，逗号分隔；Session 轮流分配")
	sessions := fs.Int("sessions", 50, "并发 Session（EndUser）数")
	duration := fs.Duration("duration", 30*time.Second, "压测时长")
	agent := fs.String("agent", "bench", "AgentDef")
	key := fs.String("key", os.Getenv("YANSHI_KEY"), "业务线私钥，为每个 EndUser 签发令牌；为空时不带令牌")
	runTimeout := fs.Duration("run-timeout", 2*time.Minute, "单个 Run 的等待上限")
	out := fs.String("out", "", "把结果写为 JSON")
	_ = fs.Parse(args)

	var signing *auth.SigningKey
	if *key != "" {
		k, err := auth.LoadSigningKey(*key)
		if err != nil {
			return err
		}
		signing = k
	}
	urls := strings.Split(*servers, ",")
	rec := &benchRecorder{start: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	var wg sync.WaitGroup
	for i := range *sessions {
		u := &benchUser{urls: urls, agent: *agent, timeout: *runTimeout, rec: rec,
			businessLine: "bench", endUser: fmt.Sprintf("bench-%d", i)}
		if signing != nil {
			u.businessLine = signing.BusinessLine
			u.token = signer(signing, "yanshi", u.endUser, time.Hour)
		}
		wg.Add(1)
		u.cur.Store(int64(i))
		go func() { defer wg.Done(); u.run(ctx) }()
	}
	progress := time.NewTicker(5 * time.Second)
	defer progress.Stop()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	for {
		select {
		case <-progress.C:
			fmt.Fprintf(os.Stderr, "%5.0fs  %s\n", time.Since(rec.start).Seconds(), rec.brief())
			continue
		case <-done:
		}
		break
	}
	res := rec.result(*sessions, len(urls))
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	if *out != "" {
		return os.WriteFile(*out, b, 0o644)
	}
	return nil
}

type benchUser struct {
	// urls 是全部 API 地址；cur 是当前使用的下标。连接失败时换到下一个，模拟负载均衡的故障转移。
	urls  []string
	cur   atomic.Int64
	agent string
	// businessLine 与 endUser 在开启鉴权时与令牌一致；-auth none 时作为自报身份。
	businessLine, endUser string
	token                 nodesdk.TokenSource
	timeout               time.Duration
	rec                   *benchRecorder
}

// 每个 EndUser 一个连接池之外的流式连接，其余请求共用连接池。
var benchClient = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1024, MaxConnsPerHost: 0}}

func (u *benchUser) base() string {
	return strings.TrimRight(u.urls[int(u.cur.Load())%len(u.urls)], "/")
}

// failover 在连接失败（进程不可达）时换到下一个 API 地址。
func (u *benchUser) failover(from string) {
	if u.base() == from {
		u.cur.Add(1)
	}
}

func (u *benchUser) do(ctx context.Context, method, path string, body any, out any) error {
	for range len(u.urls) {
		base := u.base()
		err := u.doAt(ctx, base, method, path, body, out)
		var op *net.OpError
		if !errors.As(err, &op) {
			return err
		}
		u.failover(base)
	}
	return fmt.Errorf("%s %s: no API server reachable", method, path)
}

func (u *benchUser) doAt(ctx context.Context, base, method, path string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	if err := u.authorize(req); err != nil {
		return err
	}
	resp, err := benchClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (u *benchUser) authorize(req *http.Request) error {
	if u.token == nil {
		return nil
	}
	t, err := u.token(req.Context())
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	return err
}

// benchSignal 是 SSE 中与压测相关的信号：某个 Run 的首个增量或终态。
type benchSignal struct {
	runID, status string
	delta         bool
	at            time.Time
}

func (u *benchUser) run(ctx context.Context) {
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := u.do(ctx, http.MethodPost, "/v1/sessions", map[string]string{"agent": u.agent, "business_line": u.businessLine, "end_user": u.endUser}, &created); err != nil {
		u.rec.fail("create", err)
		return
	}
	sid := created.SessionID
	signals := make(chan benchSignal, 256)
	sctx, stop := context.WithCancel(context.Background())
	defer stop()
	go u.follow(sctx, sid, signals)

	for ctx.Err() == nil {
		var sub struct {
			RunID string `json:"run_id"`
		}
		t0 := time.Now()
		// 提交使用独立 context：压测结束时不打断进行中的 Run，让它按 run-timeout 收尾。
		if err := u.do(context.Background(), http.MethodPost, "/v1/sessions/"+sid+"/inputs", map[string]string{"text": "go"}, &sub); err != nil {
			u.rec.fail("submit", err)
			time.Sleep(time.Second)
			continue
		}
		u.rec.wait(sub.RunID, t0, signals, u.timeout)
	}
}

// follow 维持 SSE 连接并转出信号；断开后带 Last-Event-ID 重连。
func (u *benchUser) follow(ctx context.Context, sid string, out chan<- benchSignal) {
	last := ""
	for ctx.Err() == nil {
		base := u.base()
		err := u.followOnce(ctx, base, sid, &last, out)
		if ctx.Err() != nil {
			return
		}
		u.failover(base)
		u.rec.fail("stream", err)
		time.Sleep(200 * time.Millisecond)
	}
}

func (u *benchUser) followOnce(ctx context.Context, base, sid string, last *string, out chan<- benchSignal) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/sessions/"+sid+"/stream", nil)
	if *last != "" {
		req.Header.Set("Last-Event-ID", *last)
	}
	if err := u.authorize(req); err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream: %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	kind := ""
	for sc.Scan() {
		line := sc.Text()
		if id, ok := strings.CutPrefix(line, "id: "); ok {
			*last = id
		} else if k, ok := strings.CutPrefix(line, "event: "); ok {
			kind = k
		} else if data, ok := strings.CutPrefix(line, "data: "); ok {
			now := time.Now()
			switch kind {
			case "delta":
				var d struct {
					RunID string `json:"run_id"`
				}
				if json.Unmarshal([]byte(data), &d) == nil {
					out <- benchSignal{runID: d.RunID, delta: true, at: now}
				}
			case "event":
				e := &v1.Event{}
				if protojson.Unmarshal([]byte(data), e) != nil {
					continue
				}
				switch p := e.GetPayload().(type) {
				case *v1.Event_RunCompleted:
					out <- benchSignal{runID: p.RunCompleted.GetRunId(), status: "completed", at: now}
				case *v1.Event_RunFailed:
					out <- benchSignal{runID: p.RunFailed.GetRunId(), status: "failed", at: now}
				case *v1.Event_RunInterrupted:
					out <- benchSignal{runID: p.RunInterrupted.GetRunId(), status: "interrupted", at: now}
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("stream closed")
}

type benchRecorder struct {
	start time.Time
	mu    sync.Mutex
	runs  []benchRun
	errs  map[string]int
	// lastErr 保留每类错误的一个样本，便于定位。
	lastErr map[string]string
}

type benchRun struct {
	status    string
	submitted time.Time
	latency   time.Duration
	ttft      time.Duration
}

func (r *benchRecorder) fail(kind string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.errs == nil {
		r.errs, r.lastErr = map[string]int{}, map[string]string{}
	}
	r.errs[kind]++
	r.lastErr[kind] = fmt.Sprint(err)
}

// wait 等待 runID 的终态；同一 Session 串行提交，因此信号只属于当前 Run 或此前的 Run。
func (r *benchRecorder) wait(runID string, t0 time.Time, signals <-chan benchSignal, timeout time.Duration) {
	run := benchRun{status: "timeout", submitted: t0}
	deadline := time.After(timeout)
loop:
	for {
		select {
		case s := <-signals:
			if s.runID != runID {
				continue
			}
			if s.delta {
				if run.ttft == 0 {
					run.ttft = s.at.Sub(t0)
				}
				continue
			}
			run.status, run.latency = s.status, s.at.Sub(t0)
			break loop
		case <-deadline:
			run.latency = timeout
			break loop
		}
	}
	r.mu.Lock()
	r.runs = append(r.runs, run)
	r.mu.Unlock()
}

func (r *benchRecorder) brief() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	recent := 0
	for _, run := range r.runs {
		if time.Since(run.submitted.Add(run.latency)) < 5*time.Second {
			recent++
		}
	}
	return fmt.Sprintf("runs=%d  last5s=%.1f/s  errors=%v", len(r.runs), float64(recent)/5, r.errs)
}

type benchResult struct {
	Sessions, Servers int
	Seconds           float64
	Runs              map[string]int
	RunsPerSecond     float64
	LatencyMs         map[string]float64
	TTFTMs            map[string]float64
	Errors            map[string]int
	ErrorSamples      map[string]string
	// Timeline 是每秒完成的 Run 数与这些 Run 的最大延迟，用于观察故障期间的曲线。
	Timeline []benchSecond
}

type benchSecond struct {
	Second       int
	Completed    int
	MaxLatencyMs float64
}

func percentiles(ds []time.Duration) map[string]float64 {
	if len(ds) == 0 {
		return nil
	}
	slices.Sort(ds)
	at := func(q float64) float64 { return float64(ds[int(q*float64(len(ds)-1))].Microseconds()) / 1000 }
	return map[string]float64{"p50": at(0.5), "p90": at(0.9), "p99": at(0.99), "max": at(1)}
}

func (r *benchRecorder) result(sessions, servers int) benchResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	res := benchResult{Sessions: sessions, Servers: servers, Runs: map[string]int{}, Errors: r.errs, ErrorSamples: r.lastErr}
	var lat, ttft []time.Duration
	var end time.Time
	seconds := map[int]*benchSecond{}
	for _, run := range r.runs {
		res.Runs[run.status]++
		done := run.submitted.Add(run.latency)
		if done.After(end) {
			end = done
		}
		if run.status != "completed" {
			continue
		}
		lat = append(lat, run.latency)
		if run.ttft > 0 {
			ttft = append(ttft, run.ttft)
		}
		sec := int(done.Sub(r.start).Seconds())
		s := seconds[sec]
		if s == nil {
			s = &benchSecond{Second: sec}
			seconds[sec] = s
		}
		s.Completed++
		s.MaxLatencyMs = max(s.MaxLatencyMs, float64(run.latency.Milliseconds()))
	}
	if !end.IsZero() {
		res.Seconds = end.Sub(r.start).Seconds()
	}
	if res.Seconds > 0 {
		res.RunsPerSecond = float64(res.Runs["completed"]) / res.Seconds
	}
	res.LatencyMs, res.TTFTMs = percentiles(lat), percentiles(ttft)
	for sec := 0; sec <= int(res.Seconds); sec++ {
		if s := seconds[sec]; s != nil {
			res.Timeline = append(res.Timeline, *s)
		} else {
			res.Timeline = append(res.Timeline, benchSecond{Second: sec})
		}
	}
	return res
}
