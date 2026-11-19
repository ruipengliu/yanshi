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
	Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), pools)
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

func gaugeVec(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: "yanshi", Name: name, Help: help}, labels)
	Registry.MustRegister(g)
	return g
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
	OutboxWatermarkLag      = gaugeVec("outbox_watermark_lag_transactions", "当前事务上界与安全水位的距离。", "shard")
	OutboxOldestTransaction = gaugeVec("outbox_oldest_transaction_seconds", "最老活跃事务的年龄。", "shard")
	OutboxConsumerLag       = gaugeVec("outbox_consumer_lag_transactions", "最近读取返回位置距安全水位的事务数；不代表业务处理确认。", "shard", "business_line")
	OutboxRows              = gaugeVec("outbox_retained_rows", "出站保留行数。", "shard")
	OutboxBytes             = gaugeVec("outbox_retained_bytes", "出站存储字节数，PG 含索引与待回收空间，内存为信封 JSON 大小。", "shard")
	ModelCallDuration       = histogram("model_call_duration_seconds", "模型调用耗时；kind = turn | summary。", latency, "kind", "outcome")
	OneshotCalls            = counter("oneshot_calls_total", "单次调用的结果；只记声明名称、版本与固定结果类别。", "name", "version", "outcome")
	ModelTokens             = counter("model_tokens_total", "模型 token 数；direction = input | output | cached_input（input 中命中前缀缓存的部分）。", "direction")
	ModelRetries            = counter("model_retries_total", "模型调用因暂时性失败（限流、服务端错误、连接中断）重试的次数。", "provider")
	// FaultsInjected 只在 serve -faults 的调试部署中出现（ADR-0033）。
	FaultsInjected      = counter("faults_injected_total", "注入的故障次数（调试部署）。", "kind")
	ModelDeadlines      = counter("model_deadlines_total", "模型调用在总时限（含排队与重试等待）内没有结果的次数。", "provider")
	ModelAdmissionWait  = histogram("model_admission_wait_seconds", "模型调用在提供商准入（速率与并发上限）处排队的时间；class = interactive | oneshot | background | background_oneshot。", latency, "provider", "class")
	RunsFinished        = counter("runs_finished_total", "进入终态的 Run；agent = name@version，用于比较灰度版本。", "status", "agent")
	RunDuration         = histogram("run_duration_seconds", "Run 从请求到终态的时长。", runDuration, "status", "agent")
	Attempts            = counter("attempts_total", "开始的 Attempt；kind = start | takeover | resume。", "kind")
	CommitConflicts     = counter("commit_conflicts_total", "追加事件时的乐观并发冲突。")
	SessionLoadEvents   = histogram("session_load_events", "加载 Session 时回放的事件数（最新快照之后的部分）。", prometheus.ExponentialBuckets(1, 2, 16))
	SessionLoad         = histogram("session_load_seconds", "认领 Session 时读取并投影日志的耗时。", latency)
	QueuePromoted       = counter("queue_promoted_total", "从到期表移回工作队列的项数。")
	QueueClaims         = counter("queue_claims_total", "Worker 认领工作队列的结果；result = ok | empty | error。", "result")
	QueueWait           = histogram("queue_wait_seconds", "工作在被认领之前已可认领多久（排队等待）；priority = interactive | resumed | background。", runDuration, "queue", "priority")
	Decisions           = counter("decisions_total", "判断（docs/design/m6-decisions.md）；outcome 为选中的选项，或 error（判断不可用，走默认路径）。", "kind", "outcome")
	DecisionDuration    = histogram("decision_duration_seconds", "一次判断的耗时。", latency, "kind")
	AdmissionRejections = counter("admission_rejections_total", "因交互工作积压过高而拒绝的新 Run（Submit 准入）。", "business_line")
	QueueReady          = gaugeVec("queue_ready", "可认领而尚未被认领的工作数（积压），由 Janitor 所在进程定期统计。", "queue", "business_line", "priority")
	QueueOldestWait     = gaugeVec("queue_oldest_wait_seconds", "积压中等得最久的工作已等待的时长，由 Janitor 所在进程定期统计。", "queue", "business_line", "priority")
	Handoffs            = counter("handoffs_total", "Worker 把 Run 移交给其他 Worker（ADR-0029）；reason = pool（交互 Worker 移交长任务）| shutdown（优雅停机）。", "reason")
	Compactions         = counter("compactions_total", "上下文压缩次数。")
	BackgroundCalls     = counter("background_calls_total", "后台调用（ADR-0035、ADR-0058）；event = backgrounded（设备离线超过宽限期，转入后台）| delegation_backgrounded（委托超过宽限期，转入后台）| result（设备结果送达）| timeout（到期）| cancelled（用户取消）。", "event")
	DisconnectWakes     = counter("disconnect_wakes_total", "设备下线时唤醒的、有调用在等它的 Session。")
	HTTPRequests        = counter("http_requests_total", "对外 API 请求。", "route", "code")
	HTTPDuration        = histogram("http_request_duration_seconds", "对外 API 请求耗时（流式接口为连接时长）。", latency, "route")
	NodesConnected      = gauge("nodes_connected", "本进程网关上已接入的 Node 连接数。")
	HandshakesRejected  = counter("handshakes_rejected_total", "因同时进行的接入握手过多而以 503 拒绝的连接（网关重启后的重连风暴）。")
	LivePeerStreams     = gauge("live_peer_streams", "本进程向其他进程拉取实时增量的连接数。")
	// 业务线名不是个人数据，可以作为标签（docs/design/m4-quota-usage.md §5）。
	UsageCost           = counter("usage_cost_micros_total", "记录的用量金额（微元）；kind = model | sandbox | call。", "business_line", "kind")
	UsageRecordErrors   = counter("usage_record_errors_total", "写入用量失败的次数（少计）。")
	ModerationChecks    = counter("moderation_checks_total", "内容安全检查；stage = input | output，result = allow | block | error。", "stage", "result")
	ModerationFailovers = counter("moderation_failovers_total", "内容安全提供商超时或出错、改问下一个提供商的次数。", "provider")
	ModerationWaits     = counter("moderation_waits_total", "输出审核不可用时的处理（ADR-0054）；result = suspended（挂起等待）| failed（超过等待上限）。", "result")
	ModerationDuration  = histogram("moderation_duration_seconds", "内容安全检查耗时。", latency, "stage")
	ModerationMock      = gauge("moderation_mock", "为 1 表示使用模拟的内容安全提供商（不得用于上线）。")
	ReleaseReloadErrors = counter("release_reload_errors_total", "发布配置重新加载被拒绝（校验失败，保留原配置）的次数。")
	QuotaUnknownTier    = counter("quota_unknown_tier_total", "档位配置已移除而回落默认额度的次数。")
	QuotaRejections     = counter("quota_rejections_total", "因配额拒绝或挂起；scope = end_user | business_line，where = submit | run | call | speech。", "scope", "where")
	// Call（docs/design/m3-call.md）
	CallsActive            = gauge("calls_active", "本进程正在中转的 Call。")
	CallsEnded             = counter("calls_ended_total", "结束的 Call；reason 同 CallEnded.reason。", "reason")
	CallAudioDropped       = counter("call_audio_dropped_frames_total", "上行音频积压超过上限而丢弃的帧（20 ms）。")
	CallTranscriptsBlocked = counter("call_transcripts_blocked_total", "未通过内容安全检查（或无法检查）的转写；role = user | assistant。", "role")
)

// PoolStats 是数据库连接池的统计来源（由 pgxpool.Pool.Stat 适配）。
type PoolStats func() (acquired, total, max int32, acquireCount, waitedCount int64, acquireTime time.Duration)

// Since 返回自 t 以来的秒数。
func Since(t time.Time) float64 { return time.Since(t).Seconds() }

// Handler 以 Prometheus 文本格式输出全部指标。
func Handler() http.Handler { return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{}) }
