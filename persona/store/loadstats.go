package store

import "context"

// LoadStats 只在显式装配的成本测试中使用，不记录正文或 owner。
// 同一事务串行使用；片段重复读取重复计数，PG 包含预取后未交给领域的片段。
type LoadStats struct {
	LedgerBytes     int64
	CaptureSegments int64
}

type loadStatsKey struct{}

func WithLoadStats(ctx context.Context, stats *LoadStats) context.Context {
	return context.WithValue(ctx, loadStatsKey{}, stats)
}

func LoadStatsFrom(ctx context.Context) *LoadStats {
	stats, _ := ctx.Value(loadStatsKey{}).(*LoadStats)
	return stats
}

func RecordLoad(ctx context.Context, ledgerBytes, captureSegments int) {
	if stats := LoadStatsFrom(ctx); stats != nil {
		stats.LedgerBytes += int64(ledgerBytes)
		stats.CaptureSegments += int64(captureSegments)
	}
}
