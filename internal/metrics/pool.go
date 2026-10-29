package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var pools = newPoolCollector()

type poolCollector struct {
	mu      sync.RWMutex
	sources map[*PoolStats]struct{}
	descs   [5]*prometheus.Desc
}

func newPoolCollector() *poolCollector {
	return &poolCollector{sources: map[*PoolStats]struct{}{}, descs: [5]*prometheus.Desc{
		prometheus.NewDesc("yanshi_db_pool_acquired_conns", "正在使用的连接。", nil, nil),
		prometheus.NewDesc("yanshi_db_pool_max_conns", "连接池上限。", nil, nil),
		prometheus.NewDesc("yanshi_db_pool_acquires_total", "获取连接的次数。", nil, nil),
		prometheus.NewDesc("yanshi_db_pool_waited_acquires_total", "因池满而等待的获取次数。", nil, nil),
		prometheus.NewDesc("yanshi_db_pool_acquire_seconds_total", "获取连接的总耗时（含因池满而等待）。", nil, nil),
	}}
}

// RegisterPool 共用一个收集器汇总活动 SQL 池，保留单库的指标名、类型与无标签形式。
// 返回的注销函数随存储关闭调用，避免重新装配时重复计数或保留已关闭的池。
func RegisterPool(stats PoolStats) func() { return pools.add(stats) }

func (c *poolCollector) add(stats PoolStats) func() {
	c.mu.Lock()
	c.sources[&stats] = struct{}{}
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() { c.mu.Lock(); delete(c.sources, &stats); c.mu.Unlock() })
	}
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range c.descs {
		ch <- desc
	}
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	if len(c.sources) == 0 {
		c.mu.RUnlock()
		return
	}
	var acquired, capacity int64
	var acquires, waited int64
	var duration time.Duration
	for stats := range c.sources {
		a, _, m, ac, wc, w := (*stats)()
		acquired += int64(a)
		capacity += int64(m)
		acquires += ac
		waited += wc
		duration += w
	}
	c.mu.RUnlock()
	values := [5]float64{float64(acquired), float64(capacity), float64(acquires), float64(waited), duration.Seconds()}
	for i, desc := range c.descs {
		kind := prometheus.CounterValue
		if i < 2 {
			kind = prometheus.GaugeValue
		}
		ch <- prometheus.MustNewConstMetric(desc, kind, values[i])
	}
}
