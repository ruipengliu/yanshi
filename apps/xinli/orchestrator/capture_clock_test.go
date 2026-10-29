package orchestrator

import "time"

// 固定接收时钟，历史夹具不随执行日期自然过期。
func captureClock() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) }
