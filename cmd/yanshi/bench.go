package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
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

// benchCmd 先准备 Session 与订阅；开放式按固定计划提交，测量中不等上一条完成。
func benchCmd(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	servers := fs.String("servers", "http://127.0.0.1:8080", "API 地址，逗号分隔；Session 轮流分配")
	sessions := fs.Int("sessions", 50, "闭环并发 Session（EndUser）数")
	duration := fs.Duration("duration", 30*time.Second, "采样时长，不含预热与收尾")
	warmup := fs.Duration("warmup", 0, "同负载预热时长，预热提交不计入报告")
	rate := fs.Float64("rate", 0, "每秒问答提交数；大于 0 时按固定计划开放式到达")
	oneshot := fs.String("oneshot", "", "并行施压的单次调用声明名")
	shotClients := fs.Int("oneshot-concurrency", 32, "单次调用的持续循环客户端数")
	agent := fs.String("agent", "bench", "AgentDef")
	key := fs.String("key", os.Getenv("YANSHI_KEY"), "业务线私钥；为空时不带令牌")
	runTimeout := fs.Duration("run-timeout", 2*time.Minute, "单次问答/单次调用的总等待上限，含 HTTP 提交")
	out := fs.String("out", "", "把结果写为 JSON")
	_ = fs.Parse(args)
	if *sessions <= 0 || *duration <= 0 || *warmup < 0 || *runTimeout <= 0 || *shotClients <= 0 || *rate < 0 || math.IsNaN(*rate) || math.IsInf(*rate, 0) || *rate > 1e6 {
		return errors.New("invalid bench parameters")
	}
	var signing *auth.SigningKey
	if *key != "" {
		k, err := auth.LoadSigningKey(*key)
		if err != nil {
			return err
		}
		signing = k
	}
	urls := strings.Split(*servers, ",")
	for i := range urls {
		urls[i] = strings.TrimSpace(urls[i])
		parsed, err := url.Parse(urls[i])
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return errors.New("servers must contain HTTP URLs")
		}
	}
	rec := &benchRecorder{duration: *duration}
	var followers sync.WaitGroup
	makeUser := func(i int) *benchUser {
		u := &benchUser{urls: urls, agent: *agent, timeout: *runTimeout, rec: rec,
			followers: &followers, businessLine: "bench", endUser: fmt.Sprintf("bench-%d", i)}
		if signing != nil {
			u.businessLine = signing.BusinessLine
			u.token = signer(signing, "yanshi", u.endUser, 24*time.Hour)
		}
		u.cur.Store(int64(i))
		return u
	}
	total := *warmup + *duration
	if total <= 0 {
		return errors.New("bench duration overflow")
	}
	count := *sessions
	if *rate > 0 {
		planned := math.Ceil(total.Seconds() * *rate)
		if planned > 100000 {
			return errors.New("open arrival plan exceeds 100000 sessions; reduce rate or duration")
		}
		count = int(planned)
	}
	// 开放式每次到达独立 Session，避免 Session 串行队列掩盖提供商准入。
	users := make([]*benchUser, count)
	streams, stopStreams := context.WithCancel(context.Background())
	defer stopStreams()
	var setup sync.WaitGroup
	jobs := make(chan int)
	for range min(count, 32) {
		setup.Go(func() {
			for i := range jobs {
				u := makeUser(i)
				u.prepare(streams)
				users[i] = u
			}
		})
	}
	for i := range count {
		jobs <- i
	}
	close(jobs)
	setup.Wait()

	start := time.Now()
	rec.start = start.Add(*warmup)
	end := start.Add(total)
	var work sync.WaitGroup
	for i := 0; *oneshot != "" && i < *shotClients; i++ {
		u := makeUser(count + i)
		work.Go(func() { u.runOneshots(end, *oneshot) })
	}
	if *rate > 0 {
		work.Go(func() { benchOpenArrivals(users, start, end, *rate, &work) })
	} else {
		for _, u := range users {
			work.Go(func() {
				for time.Now().Before(end) {
					u.runOne(time.Now())
					if u.setupErr != nil {
						time.Sleep(time.Second)
					}
				}
			})
		}
	}
	done := make(chan struct{})
	go func() { work.Wait(); close(done) }()
	progress := time.NewTicker(5 * time.Second)
	defer progress.Stop()
waiting:
	for {
		select {
		case <-progress.C:
			fmt.Fprintf(os.Stderr, "%5.0fs  %s\n", time.Since(start).Seconds(), rec.brief())
		case <-done:
			break waiting
		}
	}
	stopStreams()
	followers.Wait()
	res := rec.result(count, len(urls))
	res.Parameters = benchParameters{Servers: urls, Sessions: *sessions, Rate: *rate, Duration: duration.String(), Warmup: warmup.String(), RunTimeout: runTimeout.String(), Agent: *agent, Oneshot: *oneshot, OneshotConcurrency: *shotClients, Authenticated: signing != nil}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	if *out != "" {
		return os.WriteFile(*out, b, 0o644)
	}
	return nil
}

func benchOpenArrivals(users []*benchUser, start, end time.Time, rate float64, work *sync.WaitGroup) {
	for i, u := range users {
		due := start.Add(time.Duration(float64(i) / rate * float64(time.Second)))
		if !due.Before(end) {
			break
		}
		if delay := time.Until(due); delay > 0 {
			time.Sleep(delay)
		}
		work.Go(func() { u.runOne(due) })
	}
}

type benchParameters struct {
	Servers                                      []string
	Sessions                                     int
	Rate                                         float64
	Duration, Warmup, RunTimeout, Agent, Oneshot string
	OneshotConcurrency                           int
	Authenticated                                bool
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
	sid                   string
	signals               chan benchSignal
	setupErr              error
	followers             *sync.WaitGroup
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
		if ctx.Err() != nil {
			return ctx.Err()
		}
		base := u.base()
		err := u.doAt(ctx, base, method, path, body, out)
		var op *net.OpError
		if !errors.As(err, &op) {
			return err
		}
		u.failover(base)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("no API server reachable")
}

func (u *benchUser) doAt(ctx context.Context, base, method, path string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := u.authorize(req); err != nil {
		return err
	}
	resp, err := benchClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return benchHTTPError(resp.StatusCode)
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

type benchHTTPError int

func (e benchHTTPError) Error() string { return fmt.Sprintf("HTTP status %d", e) }

func benchFailure(err error) string {
	var status benchHTTPError
	if errors.As(err, &status) {
		if status == 429 || status == 503 {
			return fmt.Sprint(int(status))
		}
		if status == 504 {
			return "timeout"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "failed"
}

func (u *benchUser) prepare(streams context.Context) {
	ctx, cancel := context.WithTimeout(streams, u.timeout)
	defer cancel()
	var created struct {
		SessionID string `json:"session_id"`
	}
	u.setupErr = u.do(ctx, http.MethodPost, "/v1/sessions", map[string]string{"agent": u.agent, "business_line": u.businessLine, "end_user": u.endUser}, &created)
	if u.setupErr != nil {
		return
	}
	if created.SessionID == "" {
		u.setupErr = errors.New("missing session_id")
		return
	}
	u.sid = created.SessionID
	u.signals = make(chan benchSignal, 256)
	ready := make(chan struct{}, 1)
	u.followers.Go(func() { u.follow(streams, u.sid, u.signals, ready) })
	select {
	case <-ready:
	case <-ctx.Done():
		u.setupErr = ctx.Err()
	}
}

func (u *benchUser) runOne(planned time.Time) {
	t0 := time.Now()
	run := benchRun{submitted: planned, lag: max(0, t0.Sub(planned))}
	if u.setupErr != nil {
		run.status = "setup_" + benchFailure(u.setupErr)
		u.rec.record(run)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), u.timeout)
	defer cancel()
	var sub struct {
		RunID string `json:"run_id"`
	}
	err := u.do(ctx, http.MethodPost, "/v1/sessions/"+u.sid+"/inputs", map[string]string{"text": "go"}, &sub)
	if err != nil {
		run.status, run.latency = benchFailure(err), time.Since(t0)
		u.rec.record(run)
		return
	}
	if sub.RunID == "" {
		run.status, run.latency = "failed", time.Since(t0)
		u.rec.record(run)
		return
	}
	u.rec.wait(ctx, sub.RunID, t0, run, u.signals)
}

func (u *benchUser) runOneshots(end time.Time, name string) {
	for time.Now().Before(end) {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), u.timeout)
		// 只验证响应结构，不记录内容；单次调用不承诺 HTTP 重试幂等，所以每轮只发一次。
		var result struct {
			Output json.RawMessage `json:"output"`
		}
		base := u.base()
		err := u.doAt(ctx, base, http.MethodPost, "/v1/oneshots/"+url.PathEscape(name), map[string]string{"business_line": u.businessLine, "end_user": u.endUser, "input": "bench"}, &result)
		var networkError *net.OpError
		if errors.As(err, &networkError) {
			u.failover(base)
		}
		cancel()
		status := "completed"
		if err != nil {
			status = benchFailure(err)
		} else if len(result.Output) == 0 {
			status = "failed"
		}
		u.rec.recordShot(start, status)
		// 立即拒绝时有界发起，避免压测客户端的忙循环变成 CPU/RPS 瓶颈。
		if err != nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// follow 维持 SSE 连接并转出信号；断开后带 Last-Event-ID 重连。
func (u *benchUser) follow(ctx context.Context, sid string, out chan<- benchSignal, ready chan<- struct{}) {
	last := ""
	for ctx.Err() == nil {
		base := u.base()
		err := u.followOnce(ctx, base, sid, &last, out, ready)
		if ctx.Err() != nil {
			return
		}
		u.failover(base)
		u.rec.fail("stream", err)
		time.Sleep(200 * time.Millisecond)
	}
}

func (u *benchUser) followOnce(ctx context.Context, base, sid string, last *string, out chan<- benchSignal, ready chan<- struct{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/sessions/"+sid+"/stream", nil)
	if err != nil {
		return err
	}
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
		return benchHTTPError(resp.StatusCode)
	}
	select {
	case ready <- struct{}{}:
	default:
	}
	send := func(signal benchSignal) {
		select {
		case out <- signal:
		case <-ctx.Done():
		}
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
					Text  string `json:"text"`
				}
				if json.Unmarshal([]byte(data), &d) == nil && d.Text != "" {
					send(benchSignal{runID: d.RunID, delta: true, at: now})
				}
			case "event":
				e := &v1.Event{}
				if protojson.Unmarshal([]byte(data), e) != nil {
					continue
				}
				switch p := e.GetPayload().(type) {
				case *v1.Event_RunCompleted:
					send(benchSignal{runID: p.RunCompleted.GetRunId(), status: "completed", at: now})
				case *v1.Event_RunFailed:
					send(benchSignal{runID: p.RunFailed.GetRunId(), status: "failed", at: now})
				case *v1.Event_RunInterrupted:
					send(benchSignal{runID: p.RunInterrupted.GetRunId(), status: "interrupted", at: now})
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
	start    time.Time
	duration time.Duration
	shots    map[string]int
	mu       sync.Mutex
	runs     []benchRun
	errs     map[string]int
	// lastErr 只记录固定类别，不保存可能含响应内容的下游错误。
	lastErr map[string]string
}

type benchRun struct {
	status    string
	submitted time.Time
	latency   time.Duration
	ttft      time.Duration
	lag       time.Duration
}

func (r *benchRecorder) fail(kind string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.errs == nil {
		r.errs, r.lastErr = map[string]int{}, map[string]string{}
	}
	r.errs[kind]++
	r.lastErr[kind] = benchFailure(err)
}

// wait 同一 Session 串行等待；开放式的每次到达使用独立 Session。
func (r *benchRecorder) wait(ctx context.Context, runID string, t0 time.Time, run benchRun, signals <-chan benchSignal) {
loop:
	for {
		select {
		case signal := <-signals:
			if signal.runID != runID {
				continue
			}
			if signal.delta {
				if run.ttft == 0 {
					run.ttft = signal.at.Sub(t0)
				}
				continue
			}
			run.status, run.latency = signal.status, signal.at.Sub(t0)
			break loop
		case <-ctx.Done():
			run.status, run.latency = "timeout", time.Since(t0)
			break loop
		}
	}
	r.record(run)
}

func (r *benchRecorder) sampled(at time.Time) bool {
	return !at.Before(r.start) && at.Before(r.start.Add(r.duration))
}
func (r *benchRecorder) record(run benchRun) {
	if !r.sampled(run.submitted) {
		return
	}
	r.mu.Lock()
	r.runs = append(r.runs, run)
	r.mu.Unlock()
}
func (r *benchRecorder) recordShot(at time.Time, status string) {
	if !r.sampled(at) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shots == nil {
		r.shots = map[string]int{}
	}
	r.shots[status]++
}

func (r *benchRecorder) brief() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	recent := 0
	for _, run := range r.runs {
		if time.Since(run.submitted.Add(run.lag+run.latency)) < 5*time.Second {
			recent++
		}
	}
	return fmt.Sprintf("runs=%d  last5s=%.1f/s  errors=%v", len(r.runs), float64(recent)/5, r.errs)
}

type benchResult struct {
	Sessions, Servers int
	Parameters        benchParameters
	SuccessRate       float64
	TTFTSamples       int
	MissingTTFT       int
	ArrivalLagMs      map[string]float64
	Oneshots          benchShotResult
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

type benchShotResult struct {
	Outcomes          map[string]int
	Requests          int
	SuccessPerSecond  float64
	RequestsPerSecond float64
	Rejected429Ratio  float64
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
	return map[string]float64{"p50": at(0.5), "p90": at(0.9), "p95": at(0.95), "p99": at(0.99), "max": at(1)}
}

func (r *benchRecorder) result(sessions, servers int) benchResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	res := benchResult{Sessions: sessions, Servers: servers, Runs: map[string]int{}, Errors: r.errs, ErrorSamples: r.lastErr}
	var lat, ttft, lag []time.Duration
	lastSecond := int(r.duration.Seconds())
	seconds := map[int]*benchSecond{}
	for _, run := range r.runs {
		res.Runs[run.status]++
		done := run.submitted.Add(run.lag + run.latency)
		lag = append(lag, run.lag)
		if run.status != "completed" {
			continue
		}
		lat = append(lat, run.latency)
		if run.ttft > 0 {
			ttft = append(ttft, run.ttft)
		} else {
			res.MissingTTFT++
		}
		sec := int(done.Sub(r.start).Seconds())
		lastSecond = max(lastSecond, sec)
		s := seconds[sec]
		if s == nil {
			s = &benchSecond{Second: sec}
			seconds[sec] = s
		}
		s.Completed++
		s.MaxLatencyMs = max(s.MaxLatencyMs, float64(run.latency.Milliseconds()))
	}
	res.Seconds = r.duration.Seconds()
	if len(r.runs) > 0 {
		res.SuccessRate = float64(res.Runs["completed"]) / float64(len(r.runs))
	}
	res.TTFTSamples = len(ttft)
	res.ArrivalLagMs = percentiles(lag)
	res.Oneshots.Outcomes = r.shots
	for _, n := range r.shots {
		res.Oneshots.Requests += n
	}
	if res.Oneshots.Requests > 0 {
		res.Oneshots.Rejected429Ratio = float64(r.shots["429"]) / float64(res.Oneshots.Requests)
	}
	if res.Seconds > 0 {
		res.Oneshots.SuccessPerSecond = float64(r.shots["completed"]) / res.Seconds
		res.Oneshots.RequestsPerSecond = float64(res.Oneshots.Requests) / res.Seconds
	}
	if res.Seconds > 0 {
		res.RunsPerSecond = float64(res.Runs["completed"]) / res.Seconds
	}
	res.LatencyMs, res.TTFTMs = percentiles(lat), percentiles(ttft)
	for sec := 0; sec <= lastSecond; sec++ {
		if s := seconds[sec]; s != nil {
			res.Timeline = append(res.Timeline, *s)
		} else {
			res.Timeline = append(res.Timeline, benchSecond{Second: sec})
		}
	}
	return res
}
