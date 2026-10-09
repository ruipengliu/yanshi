// Package metrics 定义 yanshi 的 Prometheus 指标（docs/design/m2-scale-test.md §3）。
//
// 指标只是旁路观测，不参与任何决策，因此可以在 Worker.Step 的同步路径上直接累加，
// 不影响确定性模拟测试。
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry 是进程内全部指标的注册表；serve 在内部监听地址上通过 Handler 暴露它。
var Registry = prometheus.NewRegistry()

func init() {
	Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

func counter(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "yanshi", Name: name, Help: help}, labels)
	Registry.MustRegister(c)
	return c
}

func histogram(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: "yanshi", Name: name, Help: help, Buckets: buckets}, labels)
	Registry.MustRegister(h)
	return h
}

func gauge(name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "yanshi", Name: name, Help: help})
	Registry.MustRegister(g)
	return g
}

// latency 覆盖 1ms 到约 1 分钟；runDuration 覆盖到小时级。
var (
	latency     = prometheus.ExponentialBuckets(0.001, 2, 17)
	runDuration = prometheus.ExponentialBuckets(0.05, 2, 18)
)

var (
	ModelCallDuration = histogram("model_call_duration_seconds", "模型调用耗时；kind = turn | summary。", latency, "kind", "outcome")
	ModelTokens       = counter("model_tokens_total", "模型输入、输出 token 数。", "direction")
	RunsFinished      = counter("runs_finished_total", "进入终态的 Run；agent = name@version，用于比较灰度版本。", "status", "agent")
	RunDuration       = histogram("run_duration_seconds", "Run 从请求到终态的时长。", runDuration, "status", "agent")
	Attempts          = counter("attempts_total", "开始的 Attempt；kind = start | takeover | resume。", "kind")
	CommitConflicts   = counter("commit_conflicts_total", "Worker 追加事件时的乐观并发冲突。")
	SessionLoadEvents = histogram("session_load_events", "加载 Session 时回放的事件数（最新快照之后的部分）。", prometheus.ExponentialBuckets(1, 2, 16))
	SessionLoad       = histogram("session_load_seconds", "认领 Session 时读取并投影日志的耗时。", latency)
	QueueClaims       = counter("queue_claims_total", "Worker 认领工作队列的结果；result = ok | empty | error。", "result")
	Compactions       = counter("compactions_total", "上下文压缩次数。")
	HTTPRequests      = counter("http_requests_total", "对外 API 请求。", "route", "code")
	HTTPDuration      = histogram("http_request_duration_seconds", "对外 API 请求耗时（流式接口为连接时长）。", latency, "route")
	NodesConnected    = gauge("nodes_connected", "本进程网关上已接入的 Node 连接数。")
	LivePeerStreams   = gauge("live_peer_streams", "本进程向其他进程拉取实时增量的连接数。")
	// 业务线名不是个人数据，可以作为标签（docs/design/m4-quota-usage.md §5）。
	UsageCost           = counter("usage_cost_micros_total", "记录的用量金额（微元）；kind = model | sandbox。", "business_line", "kind")
	UsageRecordErrors   = counter("usage_record_errors_total", "写入用量失败的次数（少计）。")
	ReleaseReloadErrors = counter("release_reload_errors_total", "发布配置重新加载被拒绝（校验失败，保留原配置）的次数。")
	QuotaRejections     = counter("quota_rejections_total", "因配额拒绝或挂起；scope = end_user | business_line，where = submit | run。", "scope", "where")
)

// PoolStats 是数据库连接池的统计来源（由 pgxpool.Pool.Stat 适配）。
type PoolStats func() (acquired, total, max int32, acquireCount, waitedCount int64, acquireTime time.Duration)

// RegisterPool 暴露连接池指标：连接数、获取次数与总耗时，以及因池满而等待的次数。
func RegisterPool(stats PoolStats) {
	get := func(f func(a, t, m int32, ac, wc int64, w time.Duration) float64) func() float64 {
		return func() float64 { return f(stats()) }
	}
	Registry.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: "yanshi", Name: "db_pool_acquired_conns", Help: "正在使用的连接。"},
			get(func(a, _, _ int32, _, _ int64, _ time.Duration) float64 { return float64(a) })),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: "yanshi", Name: "db_pool_max_conns", Help: "连接池上限。"},
			get(func(_, _, m int32, _, _ int64, _ time.Duration) float64 { return float64(m) })),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Namespace: "yanshi", Name: "db_pool_acquires_total", Help: "获取连接的次数。"},
			get(func(_, _, _ int32, ac, _ int64, _ time.Duration) float64 { return float64(ac) })),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Namespace: "yanshi", Name: "db_pool_waited_acquires_total", Help: "因池满而等待的获取次数。"},
			get(func(_, _, _ int32, _, wc int64, _ time.Duration) float64 { return float64(wc) })),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Namespace: "yanshi", Name: "db_pool_acquire_seconds_total", Help: "获取连接的总耗时（含因池满而等待）。"},
			get(func(_, _, _ int32, _, _ int64, w time.Duration) float64 { return w.Seconds() })),
	)
}

// Since 返回自 t 以来的秒数。
func Since(t time.Time) float64 { return time.Since(t).Seconds() }

// Handler 以 Prometheus 文本格式输出全部指标。
func Handler() http.Handler { return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{}) }
