package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func poolValue(acquired, capacity int32, acquires, waited int64, duration time.Duration) PoolStats {
	return func() (int32, int32, int32, int64, int64, time.Duration) {
		return acquired, acquired, capacity, acquires, waited, duration
	}
}

func poolSamples(t *testing.T, registry *prometheus.Registry) map[string]bool {
	t.Helper()
	response := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	if response.Code != 200 {
		t.Fatalf("metrics status: %d", response.Code)
	}
	samples := map[string]bool{}
	for line := range strings.SplitSeq(response.Body.String(), "\n") {
		if strings.HasPrefix(line, "yanshi_db_pool_") {
			samples[line] = true
		}
	}
	return samples
}

func TestPoolMetricsSingleCompatibility(t *testing.T) {
	registry, collector := prometheus.NewRegistry(), newPoolCollector()
	registry.MustRegister(collector)
	defer collector.add(poolValue(2, 10, 7, 3, 2500*time.Millisecond))()
	samples := poolSamples(t, registry)
	// 直接断言公开文本，不给单库新增 shard 标签，也不更名或改变单位。
	want := []string{
		"yanshi_db_pool_acquired_conns 2",
		"yanshi_db_pool_max_conns 10",
		"yanshi_db_pool_acquires_total 7",
		"yanshi_db_pool_waited_acquires_total 3",
		"yanshi_db_pool_acquire_seconds_total 2.5",
	}
	if len(samples) != len(want) {
		t.Fatalf("pool metric count: %v", samples)
	}
	for _, sample := range want {
		if !samples[sample] {
			t.Fatalf("missing unchanged sample %q: %v", sample, samples)
		}
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	wantHelp := map[string]string{
		"yanshi_db_pool_acquired_conns":        "正在使用的连接。",
		"yanshi_db_pool_max_conns":             "连接池上限。",
		"yanshi_db_pool_acquires_total":        "获取连接的次数。",
		"yanshi_db_pool_waited_acquires_total": "因池满而等待的获取次数。",
		"yanshi_db_pool_acquire_seconds_total": "获取连接的总耗时（含因池满而等待）。",
	}
	for _, family := range families {
		kind := "COUNTER"
		if family.GetName() == "yanshi_db_pool_acquired_conns" || family.GetName() == "yanshi_db_pool_max_conns" {
			kind = "GAUGE"
		}
		if family.GetType().String() != kind || family.GetHelp() != wantHelp[family.GetName()] {
			t.Fatalf("pool metric schema changed: %v", family)
		}
	}
}

func TestPoolMetricsAggregateAndUnregister(t *testing.T) {
	registry, collector := prometheus.NewRegistry(), newPoolCollector()
	registry.MustRegister(collector)
	removeFirst := collector.add(poolValue(2, 10, 7, 3, time.Second))
	removeSecond := collector.add(poolValue(3, 20, 11, 5, 2*time.Second))
	defer removeFirst()
	defer removeSecond()
	samples := poolSamples(t, registry)
	for _, sample := range []string{
		"yanshi_db_pool_acquired_conns 5", "yanshi_db_pool_max_conns 30",
		"yanshi_db_pool_acquires_total 18", "yanshi_db_pool_waited_acquires_total 8",
		"yanshi_db_pool_acquire_seconds_total 3",
	} {
		if !samples[sample] {
			t.Fatalf("missing sum %q: %v", sample, samples)
		}
	}
	removeFirst()
	removeFirst()
	if samples := poolSamples(t, registry); len(samples) != 5 || !samples["yanshi_db_pool_max_conns 20"] {
		t.Fatalf("closed pool retained: %v", samples)
	}
	removeSecond()
	if samples := poolSamples(t, registry); len(samples) != 0 {
		t.Fatalf("closed pools retained: %v", samples)
	}
	defer collector.add(poolValue(1, 4, 1, 0, time.Second))()
	if samples := poolSamples(t, registry); len(samples) != 5 || !samples["yanshi_db_pool_max_conns 4"] {
		t.Fatalf("reassembly duplicated metrics: %v", samples)
	}
}
