package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"yanshi/internal/clock"
	"yanshi/internal/metrics"
	"yanshi/internal/workqueue"
)

// ErrOverloaded 表示交互工作积压过高，新 Run 暂不接受（HTTP 503，带 Retry-After）。
var ErrOverloaded = errors.New("overloaded: try again later")

// OverloadedError 带上建议的重试时刻。
type OverloadedError struct{ RetryAt time.Time }

func (e *OverloadedError) Error() string { return ErrOverloaded.Error() }
func (e *OverloadedError) Unwrap() error { return ErrOverloaded }

// Admission 决定是否接受新 Run（延展性评审 §8.4 第 5 项）。只拦新 Run：插话、带同一输入 ID 的重试、中断、
// 审批与回答都属于进行中的工作，照常接受，积压时它们的容量由此留出。
type Admission interface {
	Admit(ctx context.Context, businessLine string) error
}

// QueueAdmission 按交互工作的排队等待判定：业务线中等得最久的交互工作超过 MaxWait 时开始拒绝新 Run，
// 回落到 ResumeWait 以下才恢复（滞回：在阈值附近不来回切换）。等待取自队列的积压统计（workqueue.Stats），
// 本进程每 Refresh 至多查询一次。积压是排队时长的直接测量，不必另行估计处理速率。
type QueueAdmission struct {
	Stats               workqueue.Stats
	MaxWait, ResumeWait time.Duration
	// Refresh 默认 2 秒。
	Refresh time.Duration
	Clock   clock.Clock

	mu        sync.Mutex
	at        time.Time
	waits     map[string]time.Duration
	rejecting map[string]bool
}

func (a *QueueAdmission) Admit(ctx context.Context, businessLine string) error {
	if a == nil || a.MaxWait <= 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.Clock.Now()
	refresh := a.Refresh
	if refresh <= 0 {
		refresh = 2 * time.Second
	}
	if a.waits == nil || now.Sub(a.at) >= refresh {
		depths, err := a.Stats.Depths(ctx)
		if err != nil {
			// 统计失败时放行：准入是保护，不应成为新的单点。
			return nil
		}
		a.waits, a.at = map[string]time.Duration{}, now
		for _, d := range depths {
			if d.Class.Priority == workqueue.Interactive {
				a.waits[d.Class.BusinessLine] = max(a.waits[d.Class.BusinessLine], d.OldestWait)
			}
		}
		if a.rejecting == nil {
			a.rejecting = map[string]bool{}
		}
		for bl, on := range a.rejecting {
			if on && a.waits[bl] < a.ResumeWait {
				a.rejecting[bl] = false
			}
		}
		for bl, w := range a.waits {
			if w > a.MaxWait {
				a.rejecting[bl] = true
			}
		}
	}
	if !a.rejecting[businessLine] {
		return nil
	}
	metrics.AdmissionRejections.WithLabelValues(businessLine).Inc()
	return fmt.Errorf("%w (business line %s)", &OverloadedError{RetryAt: now.Add(refresh + time.Duration(now.UnixNano()%int64(refresh)))}, businessLine)
}
